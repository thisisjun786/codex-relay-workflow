## Method

- Frozen source: `origin/dev` at commit `b4349cc9`, checked out read-only in `/dev/shm/clitable`.
- Defined commands: the 144 top-level choices returned by `codex_session_relay.cli.build_parser()` in `packages/codex-session-relay/src/codex_session_relay/cli.py`. Nested `service` actions are options beneath the one top-level `service` command, not additional rows. The declaration cross-check grep was:
  ```regex
(?:subparsers|actions)\.add_parser\(\s*["\']([^"\']+)
  ```
- Reference scope: every regular file below `plugins/crw/skills/**` and `plugins/crw/wiring/**` in the same frozen checkout. A command is referenced only when it occupies the command position in a relay invocation, including a documented command string/argv. Prose mentions and source links such as `service.py` do not count. For each command name substituted for `<COMMAND>`, the exact grep-compatible pattern was:
  ```regex
(?:codex-session-relay|crw\s+relay)(?:\s+(?:\[)?--(?:state|socket|kind-module)(?:=|\s+)\S+(?:\])?|\s+--json)*\s+<COMMAND>(?=[\s`\'",]|$)
  ```
- File counts are distinct path counts, not occurrence counts. Paths are repository-relative; at most three are displayed.
