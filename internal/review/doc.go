// Package review is the core model of the independent code review: the finding type, the artifact (schema v1) with its validator, and
// the rules the review enforces in code rather than in prompts. It runs no process and reads no file; the runner and the pipeline
// build on it. Text a reviewer wrote is data: nothing here executes or follows it.
//
// # Rules
//
// Rules.Apply tries these rules in order; the first that fires drops the finding into Result.Dropped with its reason, as received except that an unrecognised verdict is stored as unverified.
//
//	non_finding       the title (the explanation, when there is no title) says there is nothing to report
//	noise_file        a lock file, vendored, minified or generated file (NoiseFilter)
//	invalid_location  empty, absolute, backslash or escaping path, line < 1, or endLine before line
//	not_in_diff       the cleaned path is not in Rules.Changed
//	missing_at_head   HeadReader.Lines reports fs.ErrNotExist; any other reader error makes Apply fail
//	line_beyond_head  line or endLine is past the last line of the file at head
//	unknown_grade     Grade is empty: NormalizeGrade found no grade word (a bare "security" has none)
//	threshold         Decide, below
//
// NormalizeGrade maps a reported severity to P0 to P3 and the security flag: "High security" is P1 with the flag, "critical" is P0,
// "low, high" is P1 (the most severe word wins), "security" or "bogus" has no grade.
//
// # Confidence threshold
//
// Decide takes the verdict, the support (reviewers that found it) and severe (P0, P1 or security):
//
//	confirmed                               keep
//	rejected                                drop (rejected_by_verification)
//	uncertain, support >= 2 or severe       keep
//	uncertain, support < 2 and not severe   drop (below_confidence_threshold)
//	unverified, empty or unrecognised       keep as unverified, whatever the support (fail-open)
//
// # Artifact
//
// Artifact is schema v1 and SchemaV1JSON its JSON Schema text; the Go validator (ParseArtifact, Validate, Marshal) is the authority.
// Every object has exactly the schema's keys, spelled exactly, none null; duplicate JSON keys are not detected (the last wins). A
// caller that writes JSON without Marshal bypasses the checks. The contract:
//
//	schema          crw-independent-review/1
//	tool, toolVersion, model, effort, agyVersion    not blank ("unknown" if the agy version could not be read)
//	base, head, patchId                             40 or 64 lowercase hex digits; head equals the expected head
//	diff            files, additions, deletions >= 0
//	reviewers       0 <= timedOut + authFailed <= failed <= run (a failed reviewer gave no result)
//	status, reason  complete: run >= 1, failed == 0, empty reason. partial: a reviewer returned, reason required. unavailable: none
//	                returned and no findings, reason required
//	startedAt, finishedAt   RFC 3339, finishedAt not before startedAt
//	findings        clean relative slash path, line >= 1, endLine 0 or >= line, title, explanation, perspective not blank, grade
//	                P0 to P3, verdict that passes the confidence threshold, reviewers ascending and non-empty, support equal to their count and no more
//	                than the reviewers that returned a result
//	dropped         {finding, reason, detail}: the finding (grade P0 to P3 or empty, empty for unknown_grade)
package review
