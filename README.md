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
| `models`, `modelArgs` | A model catalog and the spawn args applying it, only for an agent that reports no model selector. |
| `permissions.allow` | Tool kinds the adapter approves (below). |
| `idleTimeout` | Stop an idle session's process after this long (it resumes on the next message). |

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
- **Turns** run one at a time; input sent during a turn is queued; interrupt
  cancels the turn (`session/cancel`) and drops the queue.
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
  `mcpCapabilities.http` / `.sse`; otherwise it is dropped with a warning
  naming the server. A profile server with the same name as a configured one
  is refused (the session fails with a bad request) rather than silently
  replacing either.
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
the agent's `env`, `args` or `mode`. For the same reason `bnbloc-acp` does not
advertise bnbloc's microVM `sandboxExec`: its agents run on the host. A
teammate borrowing your ACP agent therefore runs on your machine under this
policy, unless the organization requires the sandbox for borrowed runs
(then such borrows are refused).

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
