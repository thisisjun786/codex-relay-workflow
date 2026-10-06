#!/usr/bin/env bash
# A body-only edit of a pull request (the "edited" action with no base change) reruns no job
# that already succeeded on the same head. ci.yml runs this script as the first step of
# validate, secrets and go-product on such an edit only, and guards every later step of those
# jobs with its answer. It reads the newest completed run of this workflow, of this pull
# request, of this repository, for this head, other than the run it is in, and mirrors the job
# when that run's same-named job concluded success. A go-product (test-*) job is mirrored only when
# that job's test step concluded success too: CRW-790's temporary light mode lets a leg succeed
# with its tests skipped, and such a leg must not be carried into a later run.
#
# The lookup never fails the job. No candidate, a failure, a cancellation, a skip, a missing
# job, another head, pull request, workflow or repository, an unreadable API and this run
# itself all answer mirrored=false, and the job then runs in full as it did before this script
# existed. Skipping a job is not an option: GitHub reports a skipped job's check as success, so
# a skipped dev-gate could hide an earlier red run.
set -euo pipefail

job_name=${JOB_NAME:-}
head_sha=${HEAD_SHA:-}
pull_number=${PR_NUMBER:-}
self_run=${RUN_ID:-}
repository=${REPOSITORY:-}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
output=${GITHUB_OUTPUT:-}
# The ci.yml step a go-product test leg runs, up to the leg's part; internal/dev/ci/light_mode_test.go
# holds this name and ci.yml's step name to each other, so a rename on either side is a red test.
test_step_prefix='Test and replay the contract corpus ('

mirrored=false
mirror_run=
reason='the head, pull request, repository or job name is missing'

if [[ -n $job_name && -n $head_sha && -n $pull_number && -n $self_run && -n $repository ]]; then
  reason='no completed run of this workflow for this pull request and head'
  # Every page is read and the newest candidate is picked by its own timestamp, so the answer
  # never rests on the API's page order.
  runs=$(gh api --paginate \
    "repos/$repository/actions/workflows/ci.yml/runs?head_sha=$head_sha&event=pull_request&status=completed&per_page=100" \
    --jq '.workflow_runs[] | {id, path, event, status, head: .head_sha, repository: .head_repository.full_name, pulls: [.pull_requests[]? | {number, head: .head.sha}], started: .run_started_at}' 2>/dev/null) || runs=
  # A run of this workflow is judged by what it is, not only by the request that listed it: its
  # path (a ref-qualified one is the same workflow), its event and completed state, its head
  # sha, its head repository, its pull request and its head inside that pull request. Two pull
  # requests can share a head sha and two pull requests against different bases test different
  # merge trees, so the pull request number is what makes the run this one's. A fork's run
  # carries the same head sha and would otherwise look like this repository's.
  mirror_run=$(printf '%s' "$runs" | jq -s -r \
    --arg repository "$repository" --arg head "$head_sha" --arg pull "$pull_number" --arg self "$self_run" '
    [ .[]
      | select((.path | split("@")[0]) == ".github/workflows/ci.yml")
      | select(.event == "pull_request")
      | select(.status == "completed")
      | select(.head == $head)
      | select(.repository == $repository)
      | select([.pulls[]? | select((.number | tostring) == $pull and .head == $head)] | length > 0)
      | select((.id | tostring) != $self) ]
    | sort_by(.started, .id) | last | .id // empty') || mirror_run=

  if [[ -n $mirror_run ]]; then
    reason="run $mirror_run has no successful $job_name job"
    # A re-run leaves every attempt in the list; the newest attempt is the job's answer.
    jobs=$(gh api --paginate "repos/$repository/actions/runs/$mirror_run/jobs?filter=all&per_page=100" \
      --jq '.jobs[] | {name, attempt: .run_attempt, conclusion, steps: [.steps[]? | {name, conclusion}]}' 2>/dev/null) || jobs=
    conclusion=$(printf '%s' "$jobs" | jq -s -r --arg name "$job_name" '
      [ .[] | select(.name == $name) ] | sort_by(.attempt) | last | .conclusion // empty') || conclusion=
    if [[ $conclusion == success ]]; then
      mirrored=true
      reason="run $mirror_run concluded success for $job_name"
      # A light test leg concludes success while its test step is skipped, so the leg is mirrored
      # only when that step also succeeded. The step is read from the same newest attempt the
      # conclusion came from, so an earlier attempt's green test step cannot vouch for a leg that
      # was retried and skipped its tests. A skipped, missing or absent step answers mirrored=false.
      if [[ $job_name == "go-product (test-"* ]]; then
        part=${job_name#"go-product ("}
        part=${part%)}
        step_name="${test_step_prefix}${part})"
        step_conclusion=$(printf '%s' "$jobs" | jq -s -r --arg name "$job_name" --arg step "$step_name" '
          [ .[] | select(.name == $name) ] | sort_by(.attempt) | last | (.steps // [])
          | map(select(.name == $step)) | last | .conclusion // empty') || step_conclusion=
        if [[ $step_conclusion == success ]]; then
          reason="run $mirror_run concluded success for $job_name and its test step $step_name"
        else
          mirrored=false
          reason="run $mirror_run did not conclude $job_name's test step $step_name as success (${step_conclusion:-missing})"
          mirror_run=
        fi
      fi
    else
      mirror_run=
    fi
  fi
fi
if [[ -n $output ]]; then
  printf 'mirrored=%s\nrun-id=%s\n' "$mirrored" "$mirror_run" >>"$output"
fi

if [[ $mirrored == true ]]; then
  printf 'Mirrored: %s; this body-only edit does not run %s again.\n' "$reason" "$job_name" >>"$summary"
else
  printf 'Not mirrored: %s runs in full (%s).\n' "$job_name" "$reason" >>"$summary"
fi
