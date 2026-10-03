# CXC replay status files

`internal/contracttest` replays `contract/fixtures/cxc` against the Go build (`TestDomain/cxc`) and decides each fixture by the status files in this directory: one file per port issue, so parallel port PRs never edit one shared file. `pending.json` registers all 567 fixtures as pending and is nobody's issue file.

## Format

`<name>.json`, strict JSON, with `issue` equal to the file name without `.json`:

```json
{
  "issue": "CRW-NNN",
  "identical": ["cli-help__top-level__no_arguments"],
  "intentionally-changed": [
    {"id": "cli__reset__help_is_not_help_and_resets_state", "reason": "decision 9: help no longer deletes state",
     "set": {"steps/0/stdout": "..."}, "remove": ["tree/ws/.crw/sessions"]},
    {"id": "cli__config__set_then_unset_with_manifest", "reason": "the install manifest is absorbed into crw's install record",
     "given": {"json": {"codex/.codexclaw-install.json": null}, "files": {"codex/<the file crw reads>": "..."}}}
  ]
}
```

- A non-pending claim beats a pending listing in any file. Two claims on one fixture, an id that is no fixture, an unknown field and an intentionally-changed claim without a reason are errors. A fixture no file registers fails its subtest.
- A pending fixture is not run. A claimed fixture is replayed through the four steps of [the corpus README](../../schema/cxc/README.md#replaying-against-the-go-build) and must match.
- `set` and `remove` patch the expectation. A failing replay prints the keys in crw's names: `exit`, `steps/<i>/{action,exit,signal,timeout,form,stdout,stderr}`, `tree/<path>/{type,mode,target,form,sha256,size,content}`, `calls/<i>`. `set` replaces the expected text of a key or adds one; `remove` drops the expected keys under a prefix and must match one.
- `given` (intentionally-changed only) overrides the given the build runs with, in crw's names, because some of a given has no counterpart in crw (a rewrite rule names it, R19 in four fixtures). It is an object in the grammar of a fixture's given (`files`, `json`, `symlinks`, `sqlite`, `modes`, `mtimes`, `env`, `stubs`, `fetch`, `dirs`, `git`), applied after the name substitution: a field whose value is an object has its entries set or, when `null`, removed; any other value replaces the field and `null` removes it. Entries are not merged further, and a given.json entry the override does not name keeps its key order. An unknown field (even with a `null` value) is an error. An identical claim cannot carry one: a fixture that needs it is intentionally changed.
- The CLI name table is `cli` in `contract/schema/cxc/name-substitution.json`: a cli step is mapped through its longest matching row, and a verb the table drops or does not list passes through unchanged, so only an intentionally-changed claim can make it pass.

## What the replay does with names

The expectation and the given go through `name-substitution.json` with three exceptions: a case path keeps the name of its root (`cxc/` is the engine's CXC home root, bound to `CRW_HOME`); the upstream addresses of the table's `never` list stay; text a rewrite rule names (R19, R29, R30) keeps the oracle's spelling, because no textual rule can replace it. An identical claim on such a fixture is therefore refused, and an intentionally-changed one sets or removes the key.

## SQLite

`given.sqlite` (41 databases in 37 fixtures) is seeded in Go with the `modernc.org/sqlite` driver the module already has (no cgo): each file is opened, its statements run in order, and the file is made mode 0644, whatever umask the test process has. A database the run leaves in the tree (43 entries in 35 fixtures, 6 of them created by the run itself) is read back read-only, so a WAL database keeps its `-wal` and `-shm` files, and recorded as `form: sqlite`, the text the recorder's Node dumper prints: tables ordered by type then name, an object per row in column order for a plain table (`null` rows for an index, view, trigger or virtual table), a BLOB as an object keyed by byte index. Not reproduced, and absent from the corpus: Node lists numeric-looking column names first, in numeric order. A claim compares the entry as the key `tree/<path>/content`.

## The closed network

The oracle's processes cannot reach a host: a fetch fails and is logged as a `fetch` call. A replayed case has the part of that a Go build can have: `HTTP_PROXY`, `HTTPS_PROXY` and their lower-case names point at `http://127.0.0.1:1`, where nothing listens, so the HTTP clients of the build fail instead of reaching a host (a client that ignores the variables, a loopback target, which Go does not proxy, a raw connect, a DNS lookup, and a `given` or step `env` that sets or unsets the variables are not held). Nothing is logged, so a fetch call in the expectation (6 fixtures: skill search and show without a scripted reply, and their `--help`) is a difference at `calls/<i>`: the claim is intentionally changed and removes it (`"remove": ["calls/0"]`). A scripted reply (`given.fetch`, 4 fixtures) and a connect or dns call (none today) need a log or a server this replay does not have, so they are refused.

## Refused

A non-pending claim is refused with an error naming what is missing when

- the given scripts a fetch reply (4 fixtures) or the expectation holds a `connect` or `dns` call (none does today): the closed network answers and logs nothing, so no claim can pass;
- the given the claim runs with, after its override, still holds the text of a rewrite rule (4 fixtures, R19): the override must remove it;
- the claim is identical and the expectation holds the text of a rewrite rule (33: R19, R29, R30): the oracle's spelling stays there, so the claim must set or remove that key.

## Seams the replay does not provide

A frozen clock, a network call log and V8's stack text. The nine `path_length_dependent` fixtures run in a case root of `/var/tmp/cxc-rec-<16 hex>`, the recorded 33 bytes; the oracle's plugin-root length is not reproduced (crw injects its resolved invocation), so the recall issue claims the cut recovery line as intentionally changed. A claimant adds a seam in its own issue or states the difference in an intentionally-changed claim.
