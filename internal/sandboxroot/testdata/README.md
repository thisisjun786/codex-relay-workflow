# cases.json: the shared sandbox-root case table

`cases.json` is the one table the Go rule (`internal/sandboxroot`, `TestCaseTableMatchesCheck`) and the
coordinator's Python guard (`/state/crw/mgmt/bin/codex_root_guard.py --self-test <this file>`) both run
(CRW-1265). Each case is self-contained: it names the fake tree, the fake crw `config.json`, the fake
Codex `config.toml`, the fake account home, the candidate, the sandbox and the expected verdict. Each
implementation builds the case in its own temporary directory and judges it there; a difference between
the two implementations shows up as a case one of them fails. Nothing in a case names the real
`/home/jun`, the real `~/.codex` or a real `codex`.

The top level is `{"schema": "crw-sandbox-root-cases/1", "cases": [ ... ]}`.

## Placeholders

Every string in a case (paths, the TOML text, the JSON config) may contain `{root}`. The runner replaces
it with the real path (symlinks resolved) of the temporary directory it made for the case. A path that
does not contain `{root}` is used as written (`/` and the relative paths of the `relative` cases).

## A case

| key | meaning |
| --- | --- |
| `name` | unique name; the Go tests select rows by it |
| `note` | free text, ignored |
| `account_home` | the fake passwd account home (the protected home); need not exist |
| `home_env` | optional: the fake `$HOME` of the process; default is `account_home` |
| `dirs` | directories made with `mkdir -p` |
| `git_dirs` | directories that get a `.git` directory (made with `mkdir -p`) |
| `git_files` | directories that get a `.git` file (`gitdir: /elsewhere`), the marker of a linked worktree |
| `symlinks` | `{link: target}`; the link's parent is made; the target need not exist |
| `crw_config` | the crw `config.json` document, written to `{root}/crw-config.json` and named by `CRW_CONFIG`; `null` writes no file. It carries `paths` (`sandbox_root`, `worktree_root`) and `sandbox.project_roots` |
| `codex_toml` | the text of `$CODEX_HOME/config.toml` (`CODEX_HOME` is `{root}/codex-home`, always made); `null` writes no file |
| `kind` | `check` (a working root), `writable` (a writable root: rules 1 to 5 only) or `destination` (a worktree that does not exist yet) |
| `candidate` | the path judged, as received |
| `sandbox` | the effective sandbox: a mode (`workspace-write`, `read-only`, `danger-full-access`, `dangerFullAccess`, `bypass`), `unknown` (could not be determined: on), or `""` (unspecified: Codex's default from `config.toml`: `default_permissions` is on, `sandbox_mode = "danger-full-access"` is off, a selected `profile` wins, nothing found is on) |
| `expect` | `{"verdict": "allow"}` or `{"verdict": "refuse", "rule": ..., "project": ..., "resolved": ...}`; `project` is set for `project_ancestor` (the lexicographically first project root the candidate is above); `resolved` is optional |

The process environment of a case holds exactly `HOME`, `CODEX_HOME` and `CRW_CONFIG`; every other
variable (`XDG_*`, `TMPDIR`) is empty, so `worktree_root` defaults to
`$HOME/.local/share/crw/worktrees` and `$CODEX_HOME/worktrees` is the second worktree base.

## Rules (the `rule` values)

Applied in this order; the first that applies is the answer. A sandbox that is off (`danger-full-access`,
`dangerFullAccess`, `bypass`, or `""` with an off default) is outside the rule: allow, nothing else read.

0. `sandbox_root_unusable`: `paths.sandbox_root` (default `/scratch/crw`) is not absolute, does not
   exist, is `/`, is the protected home or above it, or the crw configuration cannot be read. It refuses
   every check, writable root and destination.
1. `relative`, `missing` (never created).
2. The real path (symlinks and `..` resolved) is judged from here on.
3. `filesystem_root`.
4. `account_home`, `home_ancestor`. Protected homes are the account home and `$HOME` when `$HOME` differs
   and is not at or below the sandbox base.
5. `project_ancestor`: the candidate is a strict ancestor of a known project root (`project` names it).
   Known project roots are `sandbox.project_roots` (real paths; missing, `/` and the protected home or
   above dropped) and the keys of `[projects."<path>"]` in `config.toml` (same drops; a key at or below
   the sandbox base counts only when it has a `.git` file or directory).
6. Allowed (kind `check`): at or below the sandbox base, at or inside a known project root, or at or
   inside a linked worktree (a directory with a `.git` regular file) strictly below a worktree base.
7. Otherwise `outside_dedicated_root`.

`writable` stops after rule 5. `destination` resolves the nearest existing ancestor and appends the
rest; it refuses `/`, the protected home, its ancestors, a worktree base itself, and anything not
strictly below a worktree base, the sandbox base or a known project root (`outside_dedicated_root`).
