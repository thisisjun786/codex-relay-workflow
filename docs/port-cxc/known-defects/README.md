# CXC port: defect records by issue

The CXC port's defect records live in this directory, one file per issue. A defect found while porting
goes to the file of the issue that found it, so two pull requests never edit the same record and the
merge lane stops seeing them collide.

[docs/port-cxc/known-defects.md](../known-defects.md) is the read-only record of the port up to this
rule. It is never edited. Its opening rules still apply to the files here: a defect is added and not
fixed, with the same exceptions (the destructive `reset --help`, a security finding, and a defect that
loses or truncates settings, state or record files), and a reproduced defect keeps its line.

## File name and shape

A record file is `docs/port-cxc/known-defects/<ISSUE>.md`, named after the issue that found the defects
(for example `CRW-684.md`). That issue's pull request creates it; a later issue never edits another issue's file.

A file starts with `# <ISSUE> — <short title>`, then a `Source:` paragraph naming the oracle files and
lines at v0.2.40 (commit 3c1459ac) and the Go files of the port, then one bullet per defect. A bullet
keeps the old file's line shape, and a `fixed` or `kept` line carries its reason after the status:

```text
- <what the oracle does> (<pointer>); port: <pending|kept|fixed>.
```

## Changing an item's status

A status change never edits the old line; the issue adds a line to its own file:

```text
- Supersedes known-defects.md, section "<section heading>", item beginning "<first words>": now port: <status> (<reason>).
```

When the item is in another issue's file, name that file in place of `known-defects.md`. An item's
current status is its latest superseding record.

## Reading and edit regions

Nothing reads these files by program; Go comments that name `known-defects.md` stay as they are. Each pull
request declares its own record file as an `independent` edit region, beside the other regions the change
declares; no mechanical union is needed for the record file.
