// Package command is crw review: it reviews the change from a base to a head with the independent code review (bundle.Build, then pipeline.Run with agy.Run and a
// Git-backed head reader), writes <head>.json and its .sha256, and enforces the run rules of the terms of use: a patch-id is reviewed once (any finished record in the
// ledger ends it), at most --daily-cap reviews start per UTC day, and one review runs at a time (the run lock). The review is recorded as finished before its files are
// published, so nothing after that can let the patch be reviewed again. The issue id is data only: it never enters a prompt. docs/review/crw-review.md describes the
// flags, the ledger, the summary and the exit statuses.
package command
