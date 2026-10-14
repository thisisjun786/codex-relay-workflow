# Night batch report

A night batch is a set of Backlog issues chosen by one label and run once while nobody is watching
(CRW-1040). Its morning report is a status report in the form
[crw-status](../SKILL.md) already requires: the conclusion first, then what moved, then what is
waiting, then the evidence and what stays unverified. `crw manage batch report --digest FILE` writes
it into the management state directory at `<state_dir>/batch/reports/<finish date>-<label>-<label digest>.md`, from
the run digest the night run leaves. `<label>` is the label folded to letters, digits and dashes (at most 64
characters and 128 bytes), and the label digest is the first 32 hex digits of the sha256 of the label as the
digest gives it, so labels that differ only in punctuation or case, or share a long prefix, get different
files. The last item of every report is `라벨 식별: sha256 <hex>`, the whole sha256 of the label: the same
label on the same day is the same batch and its report is rewritten, while a file at the path that is not
a report of this label (another label whose digest collides, or a file the command did not write) is kept
and the new report is refused. Nothing in the report is written from memory of the run: an item is
placed by what the digest records.

## Sections

The report is written in Korean, short, with these headings in this order (the command and its tests
use the same four):

| Heading | What it holds |
|---|---|
| `# 밤 배치 아침 보고: <label>` then `**결론**` | the counts (selected, integrated, cancelled, carried, needing Jun's decision, unverified), the deadline and why the run ended |
| `## 통합됨` | each integrated issue with the evidence that it landed (a merge commit or a link) |
| `## 취소·이월` | each cancelled or carried issue with its reason |
| `## Jun 결정이 필요한 것` | each issue that waits on a new decision from Jun, with the question |
| `## 확인한 증거와 미확인` | the time checked, the run's start and end, each provider's observation, and every item the record does not show |

An empty section says `없음`; it is never left out.

## What counts

* An integrated item with no evidence is not counted as integrated. It is listed as unverified:
  a run that says it merged something has not shown it.
* A cancelled, carried or decision item with no reason is still placed where it belongs, with
  `이유 기록 없음`, and is listed as unverified.
* An item outside the plan's selected list is not reported as integrated whatever its outcome. It goes
  to Jun's decision section, because a release outside the batch's list is the thing the batch must not do.
  An empty `selected` list selects nothing, so every item is outside it. A digest without a `selected`
  list is refused rather than read as selecting everything.
* A selected issue with no item in the digest is unverified (`실행 기록 없음`). A repeated issue is
  counted once; the repeat is unverified. A key repeated in `selected` is one selected issue. An outcome the report does not know is unverified.
* A provider whose quota or capacity the run could not observe is shown as `관측 안 됨`, never as
  having room. A digest with no provider record says so.

## The digest

The run writes one JSON object, schema `crw-batch-digest/1`: `label`, `selected` (required: the issue keys of the
plan, `[]` when none), `started_at`, `finished_at` and `deadline` (RFC 3339, each optional), `stopped` (why the run
ended: finished, deadline, budget, stopped), `providers` (`name`, `observed`, `detail`) and `items`
(`issue`, `title`, `outcome`, `reason`, `evidence`). `outcome` is `integrated`, `cancelled`, `carried` or
`needs_decision`. An issue key (in `selected` and on an item) is a plain identifier: letters, digits, `-`,
`_` and `.`, starting and ending with a letter or digit; a digest with another key is refused. Free text
(the label, a title, a reason, evidence, `stopped`, a provider's name and detail) is shown on one line, every
line break or other white space or control run folded to one space, so it never adds a heading or an item
to the report, and `&`, `<` and `>` are written as `&amp;`, `&lt;` and `&gt;`, so HTML in it is shown as
text and never as a tag (a fake heading or item) in a viewer that allows HTML in Markdown. The plan the `selected` list comes from is `crw manage batch plan --label LABEL`, which
reads the issue JSON the caller supplies (one object per issue, with an id or identifier, a state value, labels and a body) and selects those whose state value is Backlog that carry the label and pass
the release gate (`crw skill issue-ready check`), holding the others with their reasons. An issue the list
gives twice, even once by its identifier and once by its UUID, is judged once, by its first entry; every
correspondence of an id and an identifier in the whole list is collected first, so an entry that ties the
two names, wherever it stands, makes every entry of either name the same issue, and each repeat is held as a
duplicate naming the first entry.
