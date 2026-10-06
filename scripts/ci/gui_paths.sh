#!/usr/bin/env bash
# The `gui` job's first decision (CRW-831): does this run have to verify the screens, or may it
# end without installing Node? ci.yml runs this script as the step before setup-node and gates
# every later step on its answer.
#
# The full verification runs when any watched path changed, when the changed list cannot be read,
# and on a manual dispatch. It is the safe direction: a change that is not recognized still costs
# a runner, while a screen change that is skipped would let internal/gui/assets drift from web/
# unnoticed. Light mode never reaches this decision: the gui job does not read CRW_CI_MODE, so a
# light pull request runs the screens in full, exactly like every other run.
#
# A pull request compares its base with its head, a push to dev the commit it replaced with the
# one it added. Neither base is a manual dispatch, and a push that created the branch carries the
# all-zeros before, which is not a commit: both run in full.
#
# The answer travels in $GITHUB_OUTPUT, and the job's later steps run only on `changed == 'true'`.
# A gate that cannot write that answer would therefore skip the whole screen verification while
# its job still concluded success, so a missing or unwritable output path is refused here rather
# than passed over.
set -euo pipefail

output=${GITHUB_OUTPUT:-}
if [[ -z $output ]]; then
  echo 'gui_paths.sh: GITHUB_OUTPUT is not set, so the decision cannot be recorded; refusing to end without an answer' >&2
  exit 1
fi

# The watched paths. web/ is the screen source, internal/gui/assets/ is what is committed and
# embedded, and the last four are the gui definition: the job itself, the Makefile target, this
# script and the drift check. internal/dev/ci/gui_paths_test.go holds every entry that is a file
# to its existence at the repository root, so a rename cannot leave the list pointing at nothing.
watched=(web internal/gui/assets .github/workflows/ci.yml Makefile scripts/ci/gui_paths.sh internal/dev/ci/gui_drift.go)

base=
head=
if [[ -n ${PR_BASE_SHA:-} ]]; then
  base=${PR_BASE_SHA}
  head=${PR_HEAD_SHA:-HEAD}
elif [[ -n ${PUSH_BEFORE_SHA:-} && ${PUSH_BEFORE_SHA} != 0000000000000000000000000000000000000000 ]]; then
  base=${PUSH_BEFORE_SHA}
  head=${GITHUB_SHA:-HEAD}
fi

# No base to compare with (a manual dispatch, or a push that created the branch) is a full run.
changed=true
if [[ -n $base ]]; then
  # A git failure leaves changed=true: a list that could not be read must not become a skip.
  if list=$(git diff --name-only "$base" "$head" -- "${watched[@]}" 2>/dev/null); then
    if [[ -z $list ]]; then
      changed=false
    fi
  fi
fi

printf 'changed=%s\n' "$changed" >>"$output"

if [[ $changed == true ]]; then
  echo 'the screens run in full: a watched path changed, the changed list could not be read, or this run has no base to compare with'
else
  echo 'no watched path changed; the gui job ends without installing Node'
fi
