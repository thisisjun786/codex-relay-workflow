# Dispatch case data

The case sections of [the dispatch verification reference](../../../plugins/crw/skills/crw-run/references/dispatch-verification.md)
are generated from the files here, so a pull request adds a case without editing a shared line of that
document. The document is part of the plugin payload and is committed; this directory is not.

Each section's kind is fixed:

| Section | Directory | One file holds |
|---|---|---|
| Recorded cases | `recorded/` | one table row, the line exactly as it appears in the table |
| Negative cases | `negative/` | one case block: the line starting with its bold lead `**<ID> — ` and every following line up to the next case |
| Contrast cases | `contrast/` | the same as `negative/` |

A file is named `<order>-<ID>.md`: a four-digit `<order>` (today's entries are 0010, 0020, ... in document
order) and the case's ID. Generation orders by file name, so leave room in `<order>` to insert a case
between two others.

A data file uses LF line endings (a CR is refused when the file is read), and a line of only spaces counts as a blank line wherever the generator looks for one.

A row file holds exactly one table line and one trailing newline. A block file holds its block as it
appears, inner blank lines included, with no leading or trailing blank line and one trailing newline; a
case's own follow-up paragraph stays in that case's file.

A case-block section ends with its last case: the generated region runs from the first case to the last non-blank line before the next `## ` heading, so a paragraph typed after the last case in the document is part of that case. `--check` reports it as drift and `--write` replaces it with the case's data file; text that belongs to a case goes into that case's file, and other prose goes before the first case.

After changing a file here, regenerate the document:

```sh
go run -tags dev ./cmd/crw-dev ci dispatch-cases --write
```

`crw-dev ci dispatch-cases --check`, which `crw-dev ci validate` runs, refuses a committed document that
does not match these files.
