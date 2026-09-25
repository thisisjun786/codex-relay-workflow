# Source releases

Integrate work into `dev` through reviewed PRs. A release publishes one exact
commit already on dev and fast-forwards `main` to that same commit. Do not open
a `dev -> main` promotion PR. No new merge commit is created at release time.

## Prepare and validate

The repository owner runs [Release](../.github/workflows/release.yml) from `dev`
with the full commit SHA, an unused version tag such as `v0.4.1`, and user-facing
release notes. Both the original dispatcher and a rerun's actor must be the owner.
Keep `dry_run` enabled until publication is explicitly authorized.

The source must belong to dev and include the current main history. The latest
dev-push CI run and its latest attempt for that exact SHA must have completed
successfully. A PR merge-candidate run, a manual CI dispatch, an older successful
attempt, and a cancelled run are not substitutes. If a newer dev push cancelled
the chosen commit's run, rerun that original push run and inspect its result.

Validation checks the remote tag, including annotated tags, and refuses an
existing tag that identifies another commit. It rechecks CI and ancestry before
publication. Dry-run uses only a read token and creates no tag, release or branch
update; it does not need a publication credential.

Source-release tags identify repository commits. They are independent of the
plugin manifest's payload version and the imported Python package versions.
The ordinary plugin and runtime-definition checks still verify those identities.

Validation, including dry-run, also builds the static `crw` binaries with
GoReleaser v2.18.2 (`goreleaser release --snapshot --clean`) from
[.goreleaser.yaml](../.goreleaser.yaml). The snapshot publishes nothing; it proves
the selected commit still produces every archive before any tag is written.

## Binary assets

After publication creates the source release and advances main, the `release-go`
job checks that the tag identifies the released commit and attaches the binaries
to that same release. It does not create the release, edit its notes, or move
any branch. Each archive `crw_<version>_<os>_<arch>.tar.gz` holds the `crw`
binary, the compatibility symlinks `codex-session-relay`, `codex-thread-bridge`
and `crw-completion-hook`, the repository `LICENSE`, and
`packages/codex-thread-bridge/LICENSE` for the bridge's MIT provenance.
`SHA256SUMS` lists the digest of every archive. Binaries are built with
`CGO_ENABLED=0` whatever the caller's environment says; they are not signed
or notarised.

| Target | Status |
| --- | --- |
| linux/amd64 | Built; Linux is the validated relay host platform |
| linux/arm64 | Built; same code path as linux/amd64 |
| darwin/arm64 | Built but **unvalidated** until a macOS host runs the acceptance checks |

State the darwin/arm64 status in the release notes until that macOS evidence
exists.

If `release-go` fails, the source release and main are already correct. Fix the
cause and rerun the failed job for the same tag and commit; it replaces partial
assets on that release and never republishes under another version.

## Publication credential

Configure the repository secret `RELEASE_TOKEN` before a real publication. Use an
owner-owned token restricted to this repository with Contents and Workflows write
and Actions read permissions. Workflow permission is needed when advancing main
includes workflow changes. Never commit a token or substitute a local developer
credential automatically. A missing token blocks publication before any write.

Before writing, publication reads the complete release list with the owner
credential and refuses an existing draft or conflicting release for the tag.
The workflow creates the immutable tag and GitHub source release, then advances
main without force. It does not publish to a package registry, update an installed
plugin/runtime, restart services, or deploy an application. Those operations keep
their separate authorization and verification requirements.

## Verify and recover

Read back the tag's commit, published release and main SHA. They must identify the
selected source. GitHub rules prohibit rewriting or deleting version tags and
moving main backwards or onto divergent history.

If the tag exists but release creation failed, main remains unchanged. Inspect
the failure and any draft created outside this workflow; resolve that conflict
under the owner's authorization, then rerun the same tag and commit. Do not delete
or move the immutable tag.

If the release exists but the main update failed, preserve that partial result.
After correcting the cause, rerun with the same tag and commit. An existing tag
or published release may be reused only for that commit; never replace an old
tag or rewrite published notes as recovery. An API error is not proof that a
release is absent. Do not publish again under a different version just to hide
an incomplete main update.

The workflow and [repository protections](CI.md#activation) must both be active.
Passing local fixtures proves the guards in those fixtures, not real publication
credentials, hosted execution or a completed release. The local snapshot build
proves the archive layout, not a hosted GoReleaser upload.
