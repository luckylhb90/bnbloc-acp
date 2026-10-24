# bnbloc-acp

**Any [Agent Client Protocol](https://agentclientprotocol.com) agent as a
[bnbloc](https://github.com/luckylhb90/bnbloc) core.**

Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Kimi, Cursor's CLI and
many other coding agents speak ACP (JSON-RPC 2.0 over the stdio of a
subprocess). `bnbloc-acp` is a small adapter daemon that speaks bnbloc's
[Core Protocol v1](https://github.com/luckylhb90/bnbloc/blob/main/core/protocol/types.go)
on one side and ACP on the other, so adding an agent to your bnbloc rooms is
one line of config, not code.

```
bnbloc bridge --Core Protocol v1 (loopback HTTP)--> bnbloc-acp --ACP (stdio)--> agent process
```

It ships as a signed bnbloc **desktop sidecar component**: a super-admin
publishes it once, every member's desktop app downloads and supervises it,
and each member lists the agents installed on their own machine.

## Configure your agents

`bnbloc-acp` reads `<user config dir>/bnbloc-acp/agents.json`
(macOS: `~/Library/Application Support/bnbloc-acp/agents.json`, Linux:
`~/.config/bnbloc-acp/agents.json`, Windows: `%AppData%\bnbloc-acp\agents.json`).
Edits apply immediately; an invalid edit keeps the previous config.

```json
{
  "idleTimeout": "30m",
  "agents": [
    { "name": "claude", "command": "npx", "args": ["-y", "@agentclientprotocol/claude-agent-acp"] },
    { "name": "codex", "command": "npx", "args": ["-y", "@agentclientprotocol/codex-acp"] },
    { "name": "gemini", "command": "gemini", "args": ["--acp"] },
    { "name": "opencode", "command": "opencode", "args": ["acp"],
      "permissions": { "allow": ["read", "search", "think", "edit", "delete", "move", "execute"] } }
  ]
}
```

More in [`examples/agents.example.json`](examples/agents.example.json). Launch
commands change between agent releases: check each agent's ACP documentation.
Each agent uses its own sign-in (log in to Claude Code, Codex, … as usual).

| Field | Meaning |
|---|---|
| `name` | Target id and the agent's name in rooms. |
| `command`, `args`, `env` | How to start the agent in ACP mode. |
| `authMethod` | An ACP auth method passed to `authenticate` first, for agents that require it. |
| `mode` | An ACP session mode applied to every session. |
| `mcpServers` | MCP servers passed to every session. |
| `profile` | How a session's expert profile maps onto this agent (below). |
| `envProvider` | A program run for the agent on the host, never confined, whose output joins the agent's environment (below). |
| `denyRead` | Absolute paths a sandboxed run of this agent may not read, added to the sandbox descriptor's own (below). |
| `stderrActivity` | Log lines that mark a step of the running turn (below). |
| `sandbox` | Optional: overrides the sandbox routing recognised from the command (`"opencode"`, `"claude"`) or turns it off (`"off"`) (below). |
| `models`, `modelArgs` | A model catalog and the spawn args applying it, only for an agent that reports no model selector. |
| `permissions.allow` | Tool kinds the adapter approves (below). |
| `idleTimeout` | Stop an idle session's process after this long (it resumes on the next message). |

### Files an agent's sessions still need

A session keeps the program it was created with and, after an idle stop, starts
that same program again to continue the conversation. A file removed or replaced
in between (an update by whatever manages the agent) would end the conversation.
Two things keep it:

- The core publishes what its live sessions run from in `<home>/in-use.json`: every
  path in a session's launch (the command, its arguments, its environment
  values), rewritten when a session starts or ends and every minute while the
  core runs, with `stopped: true` when it exits cleanly. Whatever removes agent
  files reads it first (the bnbloc agent manager does): a file that is listed is
  kept, a file with no current word from the core is not removed.
- When a file a session's launch names is gone anyway and the agent is registered
  now with another launch, the session continues with that one, and the log says
  so (`session continues with the agent's current launch`). The agent keeps the
  conversation in its own store; a version that cannot read it fails the
  continuation visibly instead of the session being lost at once.

### An environment for an agent (`envProvider`)

An agent sometimes needs a value that cannot be written in a file: a local proxy
that listens on a new port every time, a token that is renewed. `envProvider` is
`{"command": "...", "args": [...]}`, a program the core starts **before** the agent,
on the host and never confined, even when the session is sandboxed. It prints
`NAME=VALUE` lines, then a line `READY`, and keeps running until its stdin closes
(the core closes it when the agent's process ends, whatever the reason, and ends
its process group after a short wait). The core adds the lines to the agent's
environment: after the agent's own `env`, before the sandbox descriptor's, which
wins. It never logs a value, refuses a name it keeps from agents (`BNBLOC_ACP_*`)
and a line that is not `NAME=VALUE`, and when the provider ends before the agent the
agent ends too (its endpoint is gone); the next input starts both again.

The agent keeps its own `command`, so everything the core recognises from it, the
sandbox routing above all, applies as if there were no provider. That is the point
of the split: a sandboxed agent's network is open, so a credential it can read is a
credential it can send away. The provider is where the credential lives; the agent
is given the endpoint and, at most, a placeholder. The provider is the member's own
configuration (or a component's drop-in), the same trust as the agent's command.

### Keeping a credential from sandboxed agents (`denyRead`)

The agent sandbox keeps a confined agent's network open and denies the credential
stores it knows (SSH, cloud, git, the desktop's own). A tool on this machine that
keeps a login of its own is not on that list, and a confined agent can read it and
send it away. `denyRead` names such a path: per agent (`"denyRead": ["/abs/path"]`)
or for every agent of the core, at the top level of `agents.json` or of a drop-in
(the union of all of them applies to every agent, the member's own entries
included). It joins the descriptor's `denyRead` as the helper's `--deny-read` flag
and in the descriptor's environment. It can only restrict: a component that
declares a path can make the sandbox stricter and cannot loosen it. Paths must be
absolute.

### Agents another component manages

Other components may give the core agents without editing your file: each puts
its own `<home>/agents.d/<name>.json` (the same document as `agents.json`; only
`agents` is read). The core reads them in file name order, merged **under** your
`agents.json`: a name you define always wins, and among drop-ins the first
file keeps a name. A drop-in that does not parse, or has an invalid entry, is
skipped and its last valid copy stays; a file that is removed takes its agents
with it. `bnbloc-acp check` lists every name left out and why. The bnbloc agent
manager writes `agents.d/agent-manager.json` for the agents it installs.

Check what works:

```sh
bnbloc-acp check     # every agent: available, or why not (missing command, not signed in, …)
bnbloc-acp version
```

An agent that is not installed or not signed in is listed as unavailable with
the reason and offers no target; nothing else breaks.

## What it does

- **Models** come from the agent itself (its `model` and `thought_level`
  session options); the model picked in the room is applied to the session.
- **Messages:** streamed chunks are aggregated into completed assistant
  messages; the last message of a turn carries the model, token usage and
  cost when the agent reports them.
- **Failed turns:** a turn the agent ends with an error (a JSON-RPC error,
  an `error` stop reason, or its process exiting mid-turn) is reported
  failed by `PollTurn` (`failed`, `failMessage`, and a `failCode` such as
  `agent_error` or `agent_exited`); the text it streamed since its last
  completed message is dropped, never served as a reply.
- **Context overflow:** a failed turn whose error says the conversation is
  longer than the model's context window is reported with `failCode`
  `context_overflow`, and the next turn starts a fresh ACP session in the
  same workspace (the protocol session id stays); the overflowing session
  is never resumed.
- **Turns** run one at a time; input sent during a turn is queued; interrupt
  cancels the turn (`session/cancel`) and drops the queue.
- **Deadlines:** a session create or input may carry `deadlineMs` (the
  caller owns how long a turn may run). A turn still running then is
  cancelled (`session/cancel`; the agent process is stopped if it has not
  ended the turn 30 s later) and fails with `failCode` `deadline` ("ran out
  of time after N minutes"). Without one, a turn runs as long as the agent
  works.
- **Live activity** (capability `turnActivity`): while a turn runs,
  `PollTurn` reports `startedAtMs` and `activity`, its latest steps (at
  most 8, oldest first): each tool call as `{kind: "tool", title}` (the
  agent's title, else its kind and file name; one line, at most 120
  characters, never contents), and the agent thinking or writing as one
  `thinking` / `text` step each.
- **Waiting the agent only logs:** some agents report a state of the turn
  only in their log, never over ACP. OpenCode, when the model provider
  refuses a call (a rate limit, an overloaded provider), waits out the
  provider's `retry-after` and retries on its own, sending the client
  nothing meanwhile, so the room saw a running turn doing nothing for many
  minutes. An agent's `stderrActivity` rules turn such lines of its stderr
  into a step of the running turn: `{"match": "<RE2 regexp>", "title":
  "<step title>"}`, the first matching rule wins, and the step shows the
  rule's own title (one line, at most 120 characters), never the matched
  line (stderr stays local). For OpenCode, print its log to stderr
  (`"args": ["acp", "--print-logs", "--log-level", "WARN"]`) and add:

  ```json
  "stderrActivity": [
    { "match": "message=\"stream error\".*(?i)(rate limit|too many requests|: 429\\))",
      "title": "Waiting for the model: rate-limited, the agent retries on its own" },
    { "match": "message=\"stream error\"", "title": "The model call failed; the agent retries if it can" }
  ]
  ```
- **Deliverables:** files the agent writes in the workspace appear in the room.
- **Idle and crashes:** an idle process stops after `idleTimeout` and resumes
  the same session on the next message when the agent supports
  `session/resume` or `session/load`.

## Experts and skills (agent profiles)

`bnbloc-acp` advertises bnbloc's `agentProfile` capability: when a room runs
an expert, the session arrives with a profile (the expert's instructions,
its skills, MCP servers and their env) and the adapter maps it onto the
agent natively. The agent's own configuration (its global settings, skills
and sign-in) still loads alongside: an expert adds a role, it does not
replace the member's setup.

Per agent, the `profile` block says how:

| Field | Meaning |
|---|---|
| `instructionsVia` | `meta`: ACP `session/new` `_meta.systemPrompt.append` (agents built on the Claude Agent SDK read it). `env:<NAME>`: the instructions in the agent process env var `NAME`. `file:<path>`: written into the session workspace at `<path>`, e.g. `AGENTS.md` or `CLAUDE.md`. |
| `skillsDir` | Where skills are written: `<skillsDir>/<name>/SKILL.md` plus the skill's other files, e.g. `.claude/skills`, `.agents/skills`, `.opencode/skills`. |
| `mcp` | Pass the profile's MCP servers to the agent (default `true`). |

An agent without a `profile` block gets `{"instructionsVia": "file:AGENTS.md",
"skillsDir": ".agents/skills"}` (the cross-agent instructions file and skills
directory). Mappings known to work:

| Agent | `instructionsVia` | `skillsDir` |
|---|---|---|
| Claude Code | `meta` (or `file:CLAUDE.md`) | `.claude/skills` |
| Codex | `file:AGENTS.md` | `.agents/skills` |
| OpenCode | `file:AGENTS.md` | `.opencode/skills` |

- **Files** go only to the agent-config paths of the session workspace
  (`CLAUDE.md`, `AGENTS.md`, `.claude/skills/`, `.agents/skills/`,
  `.opencode/`, `.mcp.json`). The workspace is bnbloc's, never the member's
  repository. These files are never listed as deliverables, and they are
  read-only to the agent (it cannot rewrite its role mid-turn).
- **Env:** every bnbloc session runs in its own agent process, so the
  profile's env (and `env:<NAME>` instructions) is set when that process
  starts, and again when an idle session continues in a new process.
- **Transports:** each target lists the MCP transports its agent accepts
  (`mcpTransports`: `stdio` always, plus `http` / `sse` when the agent's ACP
  `initialize` reports `mcpCapabilities.http` / `.sse`). A target is listed
  only after its agent answered `initialize` (the availability probe), so
  the list is always the agent's own answer, refreshed with its catalog.
- **MCP servers** are added to the agent's configured `mcpServers`. An http or
  sse server goes only to an agent whose `initialize` reports
  `mcpCapabilities.http` / `.sse`; a stdio server only when its command
  resolves the way the agent starts it (an absolute or workspace-relative
  path to an executable file, or a bare name on the `PATH` of the agent's
  env or the server's own); with `mcp: false` none is passed. A profile
  server with the same name as a configured one is refused (the session
  fails with a bad request) rather than silently replacing either.
- **Missing tools are said, not silent.** A profile server that is not
  passed is logged (its name and why, never its env or headers), the next
  prompt of the ACP session starts with a note telling the agent which tools
  it does not have and why (once per change; a fresh conversation is told
  again), and the turn shows the room a step "Tools not attached: …". An
  agent that accepts a server and then fails to start it without saying so
  over ACP (OpenCode ignores such failures) cannot be detected here.
- **Secrets** (env values, MCP header and env values) are never logged and
  never written to disk; they live only in the session's memory and the agent
  process environment.
- **One role at a time:** a workspace outlives its sessions, so each
  session's profile is materialized afresh: what the adapter wrote for an
  earlier profile (its instructions file, and the skills directory, which
  holds exactly the current profile's skills) is removed first, by a record
  kept beside the workspace (`<home>/workspaces/<id>.profile.json`). A
  session without a profile wears none. Only agent-config paths are ever
  removed, never through a symlink: the agent's own files stay.
- **Resume:** a session continued after an idle stop wears the same profile:
  its files are written again and its env, `_meta` and MCP servers reach the
  new process.

## Permission policy

The adapter answers the agent's requests:

- `session/request_permission` is approved when the tool kind is in
  `permissions.allow` (default `read`, `search`, `think`, `edit`, `delete`,
  `move`; `"*"` allows all) **and** every path it names is inside the
  workspace. `execute` and `fetch` are off unless you add them.
- `fs/read_text_file` and `fs/write_text_file` are confined to the workspace
  (symlinks resolved); `.room/` (the room's shared context) and the
  agent-config files of the session's profile are read-only.
- `terminal/*` needs `execute` and a working directory inside the workspace.
- A turn that ends without a reply after a refusal fails with the reason, so
  the room sees why.
- The agent's stderr stays in the local log; it never reaches a room.

The policy covers what the agent asks the client for. An agent acting through
its own built-in tools follows its own approval settings: tighten those with
the agent's `env`, `args` or `mode`. That is the policy of a session that runs
on the host. A session can instead run in bnbloc's agent sandbox, below.

## The agent sandbox

With bnbloc's agent sandbox on (a member's mode `all`, or a borrowed run under
the organization's policy) the bridge asks the core to run a session sandboxed.
No setting is needed: the core recognises the agent's **routing** from its
command (the executable and the package an `npx` is given), the way the
sandbox helper recognises a harness from the executable.

| Agent | Routing |
|---|---|
| `opencode` | a plugin the core loads through `OPENCODE_CONFIG_DIR` (the agent cannot edit it) |
| `@agentclientprotocol/claude-agent-acp` | Claude Code's shell-prefix router (`CLAUDE_CODE_SHELL_PREFIX`) |
| `@agentclientprotocol/codex-acp` | recognised, **not implemented yet** (needs an exec server in the space) |

An agent that is none of these cannot run sandboxed: a sandboxed request for it
is refused (`bad_request`, naming the agent and why), never run on the host.
`bnbloc-acp check` shows which agents can. An agent's optional `sandbox` field
overrides the recognition (`"opencode"`, `"claude"`) or turns the sandbox off for
that agent (`"off"`).

In a sandboxed session command execution is approved unless the agent's
`permissions.allow` lists kinds (the commands are routed into the sandbox, which
is what makes them safe); a host session still needs `execute` allowed.

A sandboxed session runs the agent through `bnbloc-vmexec confine`: the OS
sandbox covers the agent **and everything it spawns**, so it writes only its
workspace and cannot read the adapter's home (the agents' config, every other
workspace). Its shell commands are routed into the room's sandbox space. The exec
token lives in a file the core renews at every input, never in the agent's
environment. The ACP `terminal` client capability is not offered (its commands
would run on the host). An agent started through `npx` gets an npm cache of its
own (the confinement keeps npm's read-only), used by confined runs only; the
first start downloads the agent into it. The session's workspace is a directory
the bridge made; deleting the workspace removes nothing in it.

A request that times out while the routing's config is first prepared leaves the
preparation running, and the retry finds it done.

Limit: the routing hooks the agent's bash tool. Another way to a shell (a
subagent, an MCP server that runs commands) is confined but runs on the host
inside the confinement, not in the VM.

## Install into bnbloc

Download the binaries from the [releases](https://github.com/luckylhb90/bnbloc-acp/releases)
(or build them, below), then as a bnbloc super-admin:

```sh
bnbloc cores publish-sidecar --name acp --version 0.1.0 --publisher <you> \
  --local-url http://127.0.0.1:7341 --listen-env BNBLOC_ACP_LISTEN --supervised-env BNBLOC_ACP_SUPERVISED \
  --binary darwin-arm64=bnbloc-acp-darwin-arm64 --binary darwin-amd64=bnbloc-acp-darwin-amd64 \
  --binary windows-amd64=bnbloc-acp-windows-amd64.exe --rollout optin
```

Members who opt in (or everyone, with `--rollout auto`) get it in their
desktop app; each writes their own `agents.json`.

It can also run as a standalone server core: start `bnbloc-acp serve` where
the agents are installed and point bnbloc at it with
`BNBLOC_CORE_ACP_URL` (+ `BNBLOC_ACP_TOKEN` / `BNBLOC_CORE_ACP_TOKEN` when not
on loopback).

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `BNBLOC_ACP_LISTEN` | `127.0.0.1:7341` | Core Protocol listen address (`--listen`). A non-loopback address requires `BNBLOC_ACP_TOKEN`. |
| `BNBLOC_ACP_HOME` | `<user config dir>/bnbloc-acp` | State directory (`--home`): `agents.json`, `workspaces/`. |
| `BNBLOC_ACP_CONFIG` | `<home>/agents.json` | Agent config file (`--config`). Missing = no agents. |
| `BNBLOC_ACP_TOKEN` | unset | Bearer every request must carry; unset = no auth (loopback only). |
| `BNBLOC_ACP_SUPERVISED` | unset | `1`: exit when stdin closes (set by the desktop supervisor). |

Variables starting with `BNBLOC_ACP_` are never passed on to the agents.

## Build and test

```sh
go test -race ./...     # includes bnbloc's Core Protocol conformance suite
go build -o bnbloc-acp .
```

A tag `vX.Y.Z` builds every platform binary in CI and attaches them to the
release. Design notes: [`docs/design.md`](docs/design.md).

## License

[Apache License 2.0](LICENSE).
