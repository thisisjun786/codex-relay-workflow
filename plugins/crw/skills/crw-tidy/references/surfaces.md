# Surfaces

What each judged surface is measured against, and what exempts it. The rules themselves belong to
[Integrations](../../crw-plan/references/integrations.md); these rows only say which test to apply.

| Surface | Applicable target | Not a defect | A clear supplement requires |
|---|---|---|---|
| `저장소` repository label | One edit-target label per implementation issue, named after the repository with the exact URL as its description ([Linear operating model](../../crw-plan/references/integrations.md#linear-operating-model)) | Non-development work and an unresolved target may leave the group empty; reference-only and legacy repositories belong in context links; a preserved historical multi-repository case is not forced to a single value | Every input that names a repository names the same one, none of them disagrees, and at least one of the agreeing inputs is the current delivery PR or the existing assignment; the value already exists in the group. A majority is not agreement. Never copy project labels onto issues |
| Explicit repository address | The owner/repo or URL kept in the issue body beside the label ([Resolve the implementation repository](../../crw-plan/references/integrations.md#resolve-the-implementation-repository)) | The same non-development and unresolved-target cases; research or design with no code target needs no remote at all | The address is quotable from the label description, the current PR or the assignment, and none of them disagree |
| Required body and linked documents | The initiative body's own definition ([Initiative body standard](../../crw-plan/references/integrations.md#initiative-body-standard)); an issue's accepted criteria and its one current delivery PR link; each linked canonical document resolves and carries the revision or updated-at the citing record claims ([Keep the two sides consistent](../../crw-plan/references/integrations.md#keep-the-two-sides-consistent)) | Reading density is a target rather than a length limit, so a compact record is not short of the rule; a project or issue write does not authorize an initiative body write; a link to a document correctly marked superseded, with its successor named, is history rather than a stale reference | The missing content exists in an accepted, linkable source and the target is inside this assignment's write authority; otherwise propose the passage |
| Project membership | Every implementation issue sits in the project its product and delivery scope belong to ([Linear operating model](../../crw-plan/references/integrations.md#linear-operating-model)); a team, a product-family label or a relation does not stand in for the project | A standalone issue the operating model allows outside a project; an issue whose membership is still an open classification decision recorded as such | The project is quotable from the issue's own delivery scope or an accepted plan, and no input disagrees. Otherwise propose it and leave the choice to the decision a midpoint check surfaces: tidy never picks among candidate projects |
| Unreflected approved decision | The canonical document carries the latest accepted decision or explicit user correction ([Linear holds canonical documents](../../crw-plan/references/integrations.md#linear-holds-canonical-documents)) | An issue status, an assistant proposal or a newer local draft is not a decision; unclear decision authority is a conflict rather than a gap | The decision carries a traceable source and date, either a Linear anchor by ID or the user's own later correction, and the passage it changes is unambiguous. Link that basis in the edit |

An issue whose body, current PR and assignment all name the same repository, with the label absent
and the value already in the group, is the ordinary gap: add the one label and leave every other
field alone. An issue with no repository anywhere, on non-development work, is the ordinary
exception, because the empty group is what the rule asks for there.

Where the body names one repository and the current PR sits in another, nothing in this table
decides which is right. Record both readings and the decision that would settle them. The same
holds when a body sentence is the only evidence: a note about where work might land is not the
execution evidence the first row asks for, so that item is unverified rather than a gap.

Separate the two quiet outcomes there. Where the record itself shows the target is still undecided,
that is the unresolved-target exception the rule already allows. Where the target may well be
settled but the corroborating evidence this table requires could not be read, or does not exist
yet, that is unverified: a definite body line with no current PR and no assignment is the ordinary
case of it. Neither outcome is a defect, and neither licenses picking a value.
