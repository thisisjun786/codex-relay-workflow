# Todo 24 / 26 evidence extraction

The leaf helpers formerly in supervisor/{pyvalue,mergeevidence,report}.go now live in
internal/relay/evidence. Mergeturn and delivery/grant use the same implementations
through the new package path, removing their imports of supervisor.

Folds:
- pythonjson.go pythonSorted into evidence.Dumps at every supervisor call site. The pair was a parity bug, not separate contracts: Python keeps integral floats and exponent spelling, does not HTML-escape strings, and follows ensure_ascii for U+2028. Live Python byte comparisons now cover these values.
- reading_subset.go readingRepr nil/string cases into evidence.Repr; malformed non-string fallback stays local because fmt.Sprint differs from Python repr.
- hierarchy.go pythonRepr nil/string/bool cases into evidence.Repr. Its map-order-specific formatting remains local; see refactor-backlog.md.

Kept separate after comparing contracts:
- hierarchy.go pythonRepr vs evidence.Repr: preferred linkage diagnostic map order differs.
- reading_subset.go readingRepr vs evidence.Repr: malformed non-string fallback differs.
- linkage_subset.go StoreLinkage.Up vs registry.Registry.Up: selector and output/error semantics differ.
- report_subset.go reportRequired/reportAssertReviewed/reportVerdictLine/parseReportVerdict/reportVersion/reportPRRef are report validation/rendering helpers, not evidence.RequiredForCandidate/CurrentReportHeads (candidate report queries). There is no shared function to fold.
