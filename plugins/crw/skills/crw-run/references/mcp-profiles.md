# MCP profiles

Every Codex thread starts the MCP servers `config.toml` defines and the installed plugins provide, whether
it calls them or not, and each sub-thread it spawns does the same. Hundreds of megabytes of helper
processes per child came from servers an issue never used. The parent therefore chooses at release which
of them the child gets, as a profile the host's execution policy declares for the child role
(`docs/role-execution-policy.md`, "A role may carry MCP profiles").

## The rule

| The issue or its packet names | Profile |
| --- | --- |
| UI or browser QA: a rendered page, a screenshot, a click path | `ui-qa` |
| a review by an external model (the `oracle` tool) | `second-opinion` |
| neither | `minimal` |

Choose from the issue and its packet, and choose the least that covers what they name. The model and the
effort of the child decide nothing here. An issue that needs both capabilities needs a profile that keeps
both servers: use the host's own, and raise a decision request when its policy declares none. These three
names are the starting set; the host's policy file is what declares them, and a name it does not declare
is refused.

## Stating it

State the profile twice, and the same name in both:

- the packet carries a line `MCP profile: <name>`, so the child and a reader know what it was given;
- the release request carries it as `child.settings.mcpProfile`, in the settings of the `child` object that a
  managed start or a DAG release passes to the host unchanged:

      "child": {"hostId": "...", "title": "...", "settings": {"model": "...", "reasoningEffort": "...", "mcpProfile": "ui-qa", ...}}

A host whose policy declares profiles for the child role admits a child only when its request states one it
declares. A request that states none, or another name, is refused at the preflight before any thread
exists (`mcp_profile_required`, `mcp_profile_unknown`): correct the request and send it again. A host whose
policy declares none for the child role refuses any profile a request states (`mcp_profile_unknown`), so the
policy file declares the profiles before a release states one.

## A child that needs more

A profile is fixed for the life of the child. The host keeps no override with a thread, so each resume
sends the profile again, and a resume of a thread the host already has loaded ignores what it sends; no
call turns one more server on for a running child. A child that finds it needs a server its profile lacks
raises a decision request and does not work around it. The parent answers by releasing the work again under
the profile it needs.

## What the host does with it (Codex 0.154.0, measured on an isolated App Server)

| Question | Result |
| --- | --- |
| Turning a config.toml server off for one thread | `config.mcp_servers.<name>.enabled=false` in `thread/start` or `thread/resume`; the thread's `mcpServerStatus/list` then reports `disabled` |
| The same for a plugin's server, or a name config.toml lacks | `thread/start` fails: "invalid transport in mcp_servers.<name>" |
| Turning a plugin's server off | only with the whole plugin, `config.plugins.<id>.enabled=false` (nested form; the dotted form with a quoted id is ignored) |
| A sub-agent (with or without a model) | inherits those overrides; a server without tools starts at its spawn, a server with tools starts when the sub-agent first calls one |
| Overrides after the thread is unloaded | not kept: a resume without them starts the whole bundle |
| One thread, config.toml servers (oracle, node_repl, a stand-in) and one plugin server | 4 helper processes, 168-185 MB; under the minimal profile 1 helper (the plugin's), 11 MB |
