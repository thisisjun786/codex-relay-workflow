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
     "set": {"steps/0/stdout": "..."}, "remove": ["tree/ws/.crw/sessions"]}
  ]
}
```

- A non-pending claim beats a pending listing in any file. Two claims on one fixture, an id that is no fixture, an unknown field and an intentionally-changed claim without a reason are errors. A fixture no file registers fails its subtest.
- A pending fixture is not run. A claimed fixture is replayed through the four steps of [the corpus README](../../schema/cxc/README.md#replaying-against-the-go-build) and must match.
- `set` and `remove` patch the expectation. A failing replay prints the keys in crw's names: `exit`, `steps/<i>/{action,exit,signal,timeout,form,stdout,stderr}`, `tree/<path>/{type,mode,target,form,sha256,size,content}`, `calls/<i>`. `set` replaces the expected text of a key or adds one; `remove` drops the expected keys under a prefix and must match one.
- The CLI name table is `cli` in `contract/schema/cxc/name-substitution.json`: a cli step is mapped through its longest matching row, and a verb the table drops or does not list passes through unchanged, so only an intentionally-changed claim can make it pass.

## What the replay does with names

The expectation and the given go through `name-substitution.json` with three exceptions: a case path keeps the name of its root (`cxc/` is the engine's CXC home root, bound to `CRW_HOME`); the upstream addresses of the table's `never` list stay; text a rewrite rule names (R19, R29, R30) keeps the oracle's spelling, because no textual rule can replace it. An identical claim on such a fixture is therefore refused, and an intentionally-changed one sets or removes the key.

## Refused for now

A non-pending claim is refused with an error naming the work of the later issue for the replayer's SQLite, rewrite-rule and network handling when the fixture needs a SQLite database (39: 37 with `given.sqlite`, 2 whose expected tree holds one), the closed network (10) or a rewrite rule in its given (4).

## Seams the replay does not provide

A frozen clock, a network call log and V8's stack text. The nine `path_length_dependent` fixtures run in a case root of `/var/tmp/cxc-rec-<16 hex>`, the recorded 33 bytes; the oracle's plugin-root length is not reproduced (crw injects its resolved invocation), so the recall issue claims the cut recovery line as intentionally changed. A claimant adds a seam in its own issue or states the difference in an intentionally-changed claim.
