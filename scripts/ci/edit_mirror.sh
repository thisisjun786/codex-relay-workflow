#!/usr/bin/env bash
# A body-only edit of a pull request (the "edited" action with no base change) reruns no job
# that already succeeded on the same head. ci.yml runs this script as the first step of
# validate, secrets and go-product on such an edit only, and guards every later step of those
# jobs with its answer. It reads the newest completed run of this workflow, of this repository,
# for this head, other than the run it is in, and mirrors the job when that run's same-named
# job concluded success.
#
# The lookup never fails the job. No candidate, a failure, a cancellation, a skip, a missing
# job, another head, workflow or repository, an unreadable API and this run itself all answer
# mirrored=false, and the job then runs in full as it did before this script existed. Skipping
# a job is not an option: GitHub reports a skipped job's check as success, so a skipped dev-gate
# could hide an earlier red run.
set -euo pipefail

job_name=${JOB_NAME:-}
head_sha=${HEAD_SHA:-}
self_run=${RUN_ID:-}
repository=${REPOSITORY:-}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
output=${GITHUB_OUTPUT:-}

mirrored=false
mirror_run=
reason='the head, repository or job name is missing'

if [[ -n $job_name && -n $head_sha && -n $self_run && -n $repository ]]; then
  reason='no completed pull_request run of this workflow for this head'
  # The workflow, the event, the head and the completed state narrow the request; the run's
  # own path and head repository are judged below, because a fork's run carries the same head
  # sha and would otherwise look like this repository's.
  runs=$(gh api "repos/$repository/actions/workflows/ci.yml/runs?head_sha=$head_sha&event=pull_request&status=completed&per_page=100" 2>/dev/null) || runs=
  # The newest candidate decides, so an older green run never covers a newer red one.
  mirror_run=$(jq -r --arg repository "$repository" --arg head "$head_sha" --arg self "$self_run" '
    [ .workflow_runs[]?
      | select(.path == ".github/workflows/ci.yml")
      | select(.event == "pull_request")
      | select(.status == "completed")
      | select(.head_sha == $head)
      | select(.head_repository.full_name == $repository)
      | select((.id | tostring) != $self) ]
    | sort_by(.run_started_at, .id) | last | .id // empty' <<<"${runs:-null}") || mirror_run=

  if [[ -n $mirror_run ]]; then
    reason="run $mirror_run has no successful $job_name job"
    # A re-run leaves every attempt in the list; the newest attempt is the job's answer.
    jobs=$(gh api "repos/$repository/actions/runs/$mirror_run/jobs?filter=all&per_page=100" 2>/dev/null) || jobs=
    conclusion=$(jq -r --arg name "$job_name" '
      [ .jobs[]? | select(.name == $name) ] | sort_by(.run_attempt) | last | .conclusion // empty' <<<"${jobs:-null}") || conclusion=
    if [[ $conclusion == success ]]; then
      mirrored=true
      reason="run $mirror_run concluded success for $job_name"
    else
      mirror_run=
    fi
  fi
fi

if [[ -n $output ]]; then
  if [[ $mirrored == true ]]; then
    printf 'mirrored=true\nrun-id=%s\n' "$mirror_run" >>"$output"
  else
    printf 'mirrored=false\n' >>"$output"
  fi
fi

if [[ $mirrored == true ]]; then
  printf 'Mirrored: %s; this body-only edit does not run %s again.\n' "$reason" "$job_name" >>"$summary"
else
  printf 'Not mirrored: %s runs in full (%s).\n' "$job_name" "$reason" >>"$summary"
fi
