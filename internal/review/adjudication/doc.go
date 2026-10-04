// Package adjudication stores parent judgments alongside saved reviewer runs and
// reports a fixed PR cohort. Nothing executes reviewer text, fetches a pull request,
// starts a model or changes a merge rule. See docs/review/adjudication.md for the
// append-only file convention, metric definitions and evidence limitations.
package adjudication
