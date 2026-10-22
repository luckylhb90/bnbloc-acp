# ACP core: any Agent Client Protocol agent as a bnbloc core (2026-10-11)

Status: implemented (2026-10-11); see Status / landing for what was verified.
Written in the bnbloc repo and moved here with the code on 2026-10-11: the
source now lives at the root of this repository, not `cmd/bnbloc-acp`.

## Intent

Many coding agents speak the [Agent Client Protocol](https://agentclientprotocol.com)
(ACP): JSON-RPC 2.0 over the stdio of a subprocess the client spawns. Claude
Code (through the `claude-agent-acp` adapter), Codex (`codex-acp`), Gemini
CLI and Qwen Code (`--acp`), OpenCode (`opencode acp`), Kimi, Cursor's CLI
and others ship it. One generic adapter turns every one of them into a
bnbloc core through configuration, with no code per agent.

## Acceptance

- A member lists the ACP agents installed on their machine in one JSON file
  (name, command, args, env, optional overrides). Each available agent
  becomes a target of the ACP core; the composer can pick it like any other
  agent.
- A configured agent whose command is not installed, or that cannot start a
  session (not signed in, broken install), is listed by the adapter as
  unavailable with the reason and contributes no target. Nothing crashes;
  it becomes available on its own once fixed (periodic re-probe, config
  reload).
- A chat drive creates a workspace directory, an ACP session in it, sends the
  prompt, and the bridge reflows each completed assistant message. Follow-ups
  reuse the session; a follow-up sent during a turn is queued behind it.
- Interrupt cancels the running turn (`session/cancel`).
- Models and reasoning levels the agent reports are the target's catalog; the
  sender's choice is applied to the session. No catalog is hardcoded.
- The agent's `session/request_permission`, `fs/*` and `terminal/*` calls
  are answered by a documented, configurable policy whose default is safe.
- Files the agent writes in the workspace are listed and readable as
  deliverables (`sessionFiles` + `sessionFileBytes`).
- The adapter passes `core/conformance` (live, over HTTP) and its own unit
  tests against a fake ACP agent; it is verified end to end against at least
  one real ACP agent available on the development machine.

## Shape: a Core Protocol v1 adapter daemon

ACP is stdio to a local subprocess. A manifest core cannot host it (each
verb is one HTTP call to a daemon), and nothing in bnbloc may spawn agent
processes except a component the desktop supervises. So the ACP core is a
**Core Protocol v1 adapter daemon**, `bnbloc-acp`: a Go binary that serves
Core Protocol v1 on loopback HTTP and speaks ACP to one agent subprocess per
session.

```
bridge --Core Protocol v1 (loopback HTTP)--> bnbloc-acp --ACP (stdio JSON-RPC)--> agent process (per session)
```

**Where it runs.** On the member's machine, as a desktop-supervised sidecar
core package (`bnbloc.core.package/v1`, runtime `sidecar`, published with
`bnbloc cores publish-sidecar`), exactly like other installed sidecars. The
desktop downloads, verifies and launches it; the bridge drives it at the
package's `sidecar.localUrl`. A server host (e.g. one serving platform
agents) can run it directly and point `BNBLOC_CORE_<NAME>_URL` at it.

**Where the source lives: originally `cmd/bnbloc-acp` in bnbloc; now its own repository, `github.com/luckylhb90/bnbloc-acp`.** It is a standalone binary that
ships outside the service images, like `cmd/bnbloc-vmexec` (also a
separately distributed helper). It is not a service (`services/` is the
compose-composed control plane, and "no core adapter runs here"), and not a
seam library (`core/` holds the interfaces and protocol packages the gateway
and bridge import; no concrete core is compiled in). It imports only
`core/protocol` for the wire types, so the wire stays one definition.

## Configuration (data, not code)

One JSON file, read at start and re-read when it changes:

```json
{
  "idleTimeout": "30m",
  "agents": [
    {
      "name": "claude",
      "command": "npx",
      "args": ["-y", "@agentclientprotocol/claude-agent-acp"],
      "env": {},
      "permissions": { "allow": ["read", "search", "think", "edit", "delete", "move"] }
    },
    { "name": "opencode", "command": "opencode", "args": ["acp"] }
  ]
}
```

| Field | Meaning |
|---|---|
| `name` | Target id and harness name (`acp/<name>` in a room). Lowercase identifier, unique. |
| `command`, `args`, `env` | How to spawn the agent in ACP mode. `env` is added to the adapter's environment (the adapter's own `BNBLOC_ACP_*` variables are never passed on). |
| `authMethod` | Optional ACP auth method id the adapter passes to `authenticate` before creating sessions, for agents that require it (the method must work non-interactively, e.g. reuse the CLI's stored login). |
| `mode` | Optional ACP session mode id applied to every session (e.g. an agent's "accept edits" mode). |
| `mcpServers` | Optional ACP `McpServer` list passed to every session verbatim. |
| `stderrActivity` | Optional rules `{match, title}`: a line of the agent's stderr matching `match` (RE2) during a turn adds the step `title` to the turn's activity (first match wins; the title is the rule's text, never the line). For agents that report a wait only in their log, e.g. OpenCode waiting out a model rate limit. |
| `models`, `modelArgs` | Override used **only when the agent reports no model selector**: the catalog to show, and the args appended at spawn with `{model}` replaced by the chosen model. Both or neither. |
| `permissions.allow` | Tool kinds the adapter approves (below). |
| `idleTimeout` | How long an idle session keeps its agent process (default `30m`, `0` = forever). |

The file is the member's (`<config dir>/bnbloc-acp/agents.json` by default,
or `--config` / `BNBLOC_ACP_CONFIG`). Example configs for common agents ship
as documentation (`examples/agents.example.json`), never as
defaults: which agents a member has, and how each is launched, is the
member's environment.

## Mapping onto the core seam

| Core Protocol v1 | ACP |
|---|---|
| `POST /workspaces` | `mkdir <workspaces>/<id>` (0700). The directory is every session's `cwd`. |
| `POST .../sessions` | spawn the agent, `initialize` (protocol 1; client capabilities `fs.readTextFile`, `fs.writeTextFile`, `terminal`), optional `authenticate`, `session/new {cwd, mcpServers}`, optional mode/model/reasoning, then `session/prompt` in the background. |
| `POST .../input` | queued `session/prompt` (one turn at a time per session, FIFO). |
| `GET .../idle`, `.../turn` | no turn running and nothing queued; the last turn's failure (JSON-RPC error, `error` stop reason, agent exit, `refusal`, or an empty `max_tokens` / `max_turn_requests` turn) with `failMessage` and an additive `failCode`. |
| `GET .../messages` | completed assistant messages, aggregated from `session/update` (below). |
| `GET .../latest` | the most recently created session of the workspace. |
| `POST .../interrupt` | `session/cancel` for the running turn; queued input is dropped. |
| `PUT /workspaces/{ws}/files/{path}` | the bridge's `.room/` context and the agent-config paths (`CLAUDE.md`, `AGENTS.md`, `.claude/skills/`, `.agents/skills/`, `.opencode/`, `.mcp.json`); any other path is refused. |
| `POST /workspaces/{ws}/sessions` `profile` | capability `agentProfile`: mapped per agent by its `profile` block (README "Experts and skills"), materialized afresh per session (the earlier profile's instructions file and skills directory removed first). |
| `GET .../files`, `GET /workspaces/{ws}/files/{path}` | the workspace's deliverables. |
| `DELETE /workspaces/{ws}` | stop its sessions' agents, remove the directory. |

Discovery advertises `interrupt`, `writeFile`, `deleteWorkspace`,
`sessionFiles`, `sessionFileBytes`. Not `sandboxExec` (see Sandbox).

**Messages.** `agent_message_chunk` text accumulates into the current
assistant message. A message completes when its ACP `messageId` changes,
when the agent moves on to anything else (a tool call, a plan, a mode
change), or when the turn ends. Only completed messages are served; there is
no token streaming. `agent_thought_chunk` text becomes the next completed
message's `thinking`. The turn's last message carries `metrics`: `model` (the
session's current model), `inputTokens` / `outputTokens` when the prompt
response reports usage, `costUsd` when the agent reports a USD session cost
(the delta over the turn). Without token usage the metrics say
`reported: false`.

**Failed turns.** An errored turn is a failed turn, never a reply: when the
prompt ends in a JSON-RPC error, an `error` stop reason (not an ACP stop
reason, but some agents use it) or the agent process exiting, the turn's
status is `failed` with `failMessage` (the agent's own error message, never
its stderr) and `failCode` (`agent_error`, `agent_exited`, `session_lost`;
`refusal`, `no_reply`, `permission_refused` for the turns that end without
a reply). The text streamed since the last completed message is dropped
with its thinking, under the same lock that records the failure, so no poll
order can serve it as a completed reply. Messages that completed earlier in
the turn (before a tool call) were already served as progress. `failCode`
is an additive field beside Core Protocol v1's `PollTurnResponse`.

**Profiles in a reused workspace.** A workspace outlives its sessions
(bnbloc's core-providers guide), so each session's profile is materialized
afresh. The adapter records what it wrote for profiles in
`<workspaces>/<id>.profile.json` (outside the workspace, so neither the
agent nor a deliverables listing sees it, and it survives adapter
restarts): the instructions file and the skills directory. Before writing a
session's profile it removes the recorded instructions file (unless it is
rewritten) and the recorded and current skills directories, which then hold
exactly the profile's skills; a session without a profile removes them and
the record. Only agent-config paths are removed; a path whose parent
resolves through a symlink is refused, and a symlink at the path itself is
removed as a link, so the agent's own files are never touched.

**Sessions and processes.** One agent process per session (crash-isolated,
own `cwd`, own spawn args). When the agent can continue a session
(`session/resume`, else `session/load` with its history replay discarded),
an idle session's process stops after `idleTimeout` and the next input
respawns it and continues the same ACP session; a crashed process is
continued the same way. An agent that can do neither keeps its process while
the session lives; if that process dies, the session is lost: its last
(failed) turn status is served once, then the session answers
`session_not_found` so the bridge rebinds a fresh session, carrying the room
context through `.room/` as for any core. Adapter restarts lose sessions the
same way (workspaces persist on disk).

## Permission policy

Drives are unattended (nobody is at the agent's terminal), so every ACP
request the agent makes of the client is answered by policy:

- **`session/request_permission`**: approved when the tool call's `kind` is
  in the agent's `permissions.allow` AND every location it names is inside
  the session workspace; otherwise rejected. Default `allow`: `read`,
  `search`, `think`, `edit`, `delete`, `move`. Not by default: `execute`,
  `fetch`, `switch_mode`, `other` (an operator adds them, or `"*"` for all).
  Approval selects the agent's `allow_once` option (never `allow_always`, so
  every call is re-checked), rejection `reject_once`; with no matching option
  the answer is `cancelled`.
- **`fs/read_text_file`, `fs/write_text_file`**: always confined to the
  session workspace (symlinks resolved), reads need `read` and writes `edit`
  in `allow`; `.room/` is read-only to the agent (it is the bridge's
  context record).
- **`terminal/*`**: `terminal/create` needs `execute` in `allow` and a `cwd`
  inside the workspace (default: the workspace). The command runs on the
  host as the adapter's user (directly when `args` are given, through the
  platform shell when only a command line is given); output is kept to the
  requested byte limit; `kill`/`release` and session end kill its process
  group.
- Anything else the agent calls (e.g. `elicitation/create`, which the
  adapter does not advertise) is `method not found`.
- A turn that ends without any reply after the policy refused something
  fails with the refusals as its reason, so the room sees why instead of
  nothing.
- The agent's stderr stays local (adapter log, `bnbloc-acp check`): it is
  arbitrary output, so it never reaches an error that can be posted to a
  room. A `stderrActivity` rule only selects its own fixed title.

The policy governs what an agent asks of its client. An agent can still act
natively (its own file and shell tools) under its own approval settings;
the policy is not a sandbox, and `mode` / the agent's own config is where an
operator tightens that.

## Sandbox integration

Advertised (`sandboxExec`, where `bnbloc-vmexec` can confine: macOS and Linux)
and answered per agent. The design is `docs/prd/2026-10-22-acp-sandbox.md` in
the bnbloc repository; it replaces the earlier decision not to advertise, which
rested on one fact: an ACP agent may run shell commands itself rather than
through client terminals, so a generic adapter cannot force the routing of its
commands into a space. That is true of the routing and not of the confinement:
the OS sandbox covers the agent process and everything it spawns.

- An agent is configured with the routing its CLI has (`sandbox`, one today:
  `opencode`, a `tool.execute.before` plugin that hands the bash tool's command
  to `vmexec hook opencode`). An agent without one cannot run sandboxed; the
  request is refused and nothing runs on the host.
- The workspace is the sandbox workdir the bridge created
  (`CreateWorkspaceRequest.workdir`), adopted, never owned: delete forgets the
  record and leaves the directory. A workdir workspace takes only sandboxed
  sessions and `execution.sandbox.workdir` must be it.
- The agent is started `vmexec confine --harness … -- <agent> <args>` in the
  workdir with the descriptor's environment (`protocol.VMExecEnv`), the token
  only as a file renewed at every input, `PWD` set, the adapter's home
  read-denied (its own token directory re-allowed) and the routing's config
  directory write-denied. The client terminal capability is not offered.
- The session pins what a spawn depends on (the descriptor but for the token): an
  input with another pin continues the session in a new process (resume or load),
  or is refused when the agent can do neither.
- Known limit: commands that do not go through the routing's hook (a subagent, an
  MCP server that runs commands) are confined but run on the host.

## Model catalogs

From the agent itself: the adapter probes each configured agent (spawn,
`initialize`, `session/new` in a scratch directory, stop) at start and
periodically (unavailable agents every minute, available ones every ten),
and every real session refreshes the snapshot (`session/new` response,
`config_option_update`). Session config options with category `model` give
`models` + `defaultModel`; category `thought_level` gives `reasoningEfforts`
+ `defaultReasoning`. Grouped selects are flattened. A chosen model or
reasoning level is applied with `session/set_config_option` at session start
and when a follow-up changes it. Only an agent that reports no model
selector uses the config's `models`/`modelArgs`. Target listing answers from
the snapshot (cheap).

## Process and endpoint settings

| Setting | Default | Meaning |
|---|---|---|
| `--listen` / `BNBLOC_ACP_LISTEN` | `127.0.0.1:7341` | Listen address (the package's `listenEnv`). A non-loopback address requires a token. |
| `--home` / `BNBLOC_ACP_HOME` | `<user config dir>/bnbloc-acp` | Holds `agents.json` and `workspaces/`. |
| `--config` / `BNBLOC_ACP_CONFIG` | `<home>/agents.json` | Agent config file. Missing = no agents (the core lists, with no targets). |
| `BNBLOC_ACP_TOKEN` | unset | Bearer every request must carry when set. |
| `BNBLOC_ACP_SUPERVISED` | unset | `1`: exit when stdin closes (the package's `supervisedEnv`). |

`bnbloc-acp check` probes the configured agents and prints each one's
availability, reason, models and reasoning levels.

## Verification

- Unit tests against a fake ACP agent (the test binary re-executed as an ACP
  agent): message aggregation, queueing, interrupt, failure reporting,
  model/reasoning application, permission/fs/terminal policy, idle reaping
  with resume, unavailable agents, config validation.
- `core/conformance.RunLive` against the adapter over HTTP with the fake
  agent.
- End to end against a real ACP agent on the development machine (recorded
  below with exactly what ran).

## Status / landing

- PRD: 3a84b3f. Adapter and tests: a7c4edb. Docs (core providers guide,
  CLI publishing, environment variables, READMEs): the commit after it.
- Unit tests (`go test ./...`, also under `-race`): the test
  binary re-executed as a fake ACP agent covers aggregation, queueing,
  interrupt, failures (JSON-RPC error, refusal, crash, policy refusal),
  model/reasoning/mode application, config-declared catalogs, the
  permission/fs/terminal policy, crash resume, idle stop + `session/load`
  (replay discarded), lost sessions, auth methods, stderr locality, workspace
  files and room context, sandbox refusal, bearer auth, config validation
  and reload. `TestConformance` runs `core/conformance.RunLive` over HTTP.
- Real agents on the development machine (2026-10-11):
  - **OpenCode 1.18** (`opencode acp`, its stored provider login):
    `core/conformance` live suite (`TestRunLive_againstLocalCoreProtocolDaemon`
    against the running adapter) passed; a drive created a workspace, wrote
    `.room/`, ran a turn in which the agent wrote `hello.txt` (listed and
    read back as a deliverable), the reply arrived as one completed message
    with model and token metrics; a follow-up on the same session recalled
    the first turn; an interrupt during a long answer cancelled it within a
    second. With the agent set to ask (`OPENCODE_PERMISSION` in `env`), its
    `execute` request was refused by the default policy (the turn failed
    with the refusal as its reason) and approved once `execute` was allowed;
    an `edit` inside the workspace was approved. Its 21 models were the
    target's catalog.
  - **Codex** (`npx -y @agentclientprotocol/codex-acp`): probe available,
    its 9 models listed; a drive with a chosen model applied it through
    `session/set_config_option` (the turn's metrics carry it). The turn
    itself returned the provider's error as text (the machine's API key had
    no budget left), so no file-writing turn was verified.
  - **Claude** (`npx -y @agentclientprotocol/claude-agent-acp` 0.89.1):
    initialize answered, `session/new` failed inside the agent (its own CLI
    launch errored on this machine), so the adapter listed it unavailable
    with the agent's error, as designed. No Claude turn ran.
- Not done: `sandboxExec` (see Sandbox); per-turn token usage depends on
  the agent reporting it in the prompt response; adapter restarts lose
  sessions (the bridge rebinds).
