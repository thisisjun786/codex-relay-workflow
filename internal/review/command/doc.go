// Package command is crw review: it reviews the change from a base to a head with the independent code review (bundle.Build, then pipeline.Run with agy.Run and a
// Git-backed head reader), writes <head>.json and its .sha256, and enforces the run rules of the terms of use: a patch-id is reviewed once (any finished record in the
// ledger ends it), at most --daily-cap reviews start per UTC day, and one review runs at a time (the run lock). The review is recorded as finished before its files
// are published, so nothing after that can let the patch be reviewed again; a review that could not run at all for a reason of the account or the configuration is recorded as unavailable instead, and the patch may be
// tried once more on a later UTC day. With --post-summary the pull request's one summary comment is kept up to date (forge.go). The issue id is data only: it never enters a prompt. See docs/review/crw-review.md.
package command
