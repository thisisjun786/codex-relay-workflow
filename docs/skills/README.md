# Skill inventory map

`inventory-map.json` gives each of the 83 overlap items of the skill inventory (collisions 14, duplicateContent 16, roleMixing 19, triggerAmbiguity 14, sizeHotspots 6, deadOrStale 14) one row keyed `<category>[<index>]`. `inventory-ids.json` is the pinned copy of those identifiers. The checks live in `internal/dev/ci` (built with `-tags dev`).

## Row fields

| Field | Meaning |
| --- | --- |
| `disposition` | `fix`, `split`, `delete`, or `keep` (a keep row states its `reason`) |
| `owner` | the one issue that changes the row's text last (S01-S07); it alone flips `status` to `done` |
| `contributors` | earlier issues that change text the row names |
| `check` | exact Go test function names (a prefix is not a name) |
| `status` | `open` or `done`; a row owned by S01 starts `done` |
| `triggers` | `{phrase: owning skill, or "none"}` for `TestTriggerOverlapTable` |
| `paths` | what a row-scoped scan reads (`TestNoStaleCxcReferences`, `TestNoChangeHistoryNotes`) |
| `assert` | `{path, pattern, want}` with `want` of `present`, `absent` or `count N`, or `{path, want}` with `exists` or `missing`; `guard: true` marks text that must stay, which is not evidence |
| `sameAs` | rows whose checks cover this one |

An assert pattern is a Go regular expression behind `(?m)`, run on the whole file (every regular file below a directory path); `^` and `$` match at line starts and ends, `present` needs one match or more, `absent` none, `count N` exactly N summed over the files, and a missing file has no match.

## Checks

- `TestSkillInventoryMapComplete` (completion mode): the identifiers equal `inventory-ids.json`; every row has a disposition, an owner and a check; a done fix, split or delete row has a check test registered in `rowTestReads` (`row_test_reads.go`) with one of its paths, a non-guard assert, or `sameAs` rows that are all done and end, through their own `sameAs` rows, in a row with such evidence or a keep row (rows that name each other certify nothing); every test a done row names exists as `func <Name>(t *testing.T)` in the registered package (read with `go/parser`) and every registered read path is a Go string literal of the file that declares the test; every done row's asserts hold and its `sameAs` rows are done. An issue that turns a row to done adds its paths, asserts and its own test's registration in the same change. The completion mode (no open row is allowed) is on since S07 (CRW-1222), the last issue of the series.
- `crw-dev ci validate` (`InventoryBaseErrors`): every non-guard assert of every fix, split or delete row, open or done, is evaluated on the tree at `base` with git; an assert that already holds there is an error, and a `base` commit that is not in the repository is an error, never a skip (the validate job checks out the full history).
- `TestTriggerOverlapTable`, `TestNoStaleCxcReferences`, `TestNoChangeHistoryNotes`: read only done rows; a done row that names a scan test without paths fails.
- `TestSkillDescriptionLength`: reads the `descriptions` table (the 34 skills at `base`, less the two that CRW-1218 merged into crw-status and crw-check and the Luna search skill that CRW-1220 merged into crw-search: 31 entries): a done entry and a skill with no entry must have a description of at most 300 characters; `triggerAmbiguity[13]` done needs the entry of the skill each of its paths lies in (the directory, a file or a deeper path) done, and a path that is outside the skills or names no skill is an error; `sizeHotspots[4]` done needs every entry done. It is registered in `rowTestReads` for those two rows alone (`Rows`), so it is evidence for no other row.
