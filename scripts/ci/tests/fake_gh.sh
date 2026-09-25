#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$GH_LOG"
state_get() {
  python3 - "$GH_STATE" "$1" "$2" <<'PY'
import json, sys
from pathlib import Path
state, name, key = sys.argv[1:]
value = json.loads(Path(state, name).read_text()).get(key, "")
if value is True:
    value = "true"
elif value is False:
    value = "false"
print(value)
PY
}
if [[ "${1:-}" == api ]]; then
  shift
  if [[ "${1:-}" == --paginate ]]; then shift; fi
  query="${1:-}"
  if [[ "$query" == *actions/workflows/ci.yml/runs* ]]; then
    case=$(state_get ci.json case)
    sha=$(state_get ci.json sha)
    [[ -n "$sha" ]] || sha="$RELEASE_SHA"
    if [[ "$case" == error ]]; then exit 1; fi
    if [[ "$case" == missing ]]; then printf '%s\n' '{"workflow_runs":[]}'; exit 0; fi
    if [[ "$case" == latest-cancelled ]]; then
      printf '{"workflow_runs":[{"id":11,"run_number":1,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"},{"id":12,"run_number":2,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"cancelled"}]}\n' "$sha" "$sha"
      exit 0
    fi
    if [[ "$case" == latest-attempt-failed ]]; then
      printf '{"workflow_runs":[{"id":21,"run_number":4,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"},{"id":21,"run_number":4,"run_attempt":2,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"failure"}]}\n' "$sha" "$sha"
      exit 0
    fi
    if [[ "$case" == stale-attempt ]]; then
      printf '{"workflow_runs":[{"id":31,"run_number":5,"run_attempt":2,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"failure"},{"id":31,"run_number":5,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"}]}\n' "$sha" "$sha"
      exit 0
    fi
    if [[ "$case" == paged-pending ]]; then
      printf '{"workflow_runs":[{"id":42,"run_number":9,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"in_progress","conclusion":null}]}\n' "$sha"
      printf '{"workflow_runs":[{"id":41,"run_number":8,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"}]}\n' "$sha"
      exit 0
    fi
    if [[ "$case" == paged-success ]]; then
      printf '{"workflow_runs":[{"id":52,"run_number":7,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"}]}\n' "$sha"
      printf '{"workflow_runs":[{"id":51,"run_number":6,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"}]}\n' "$sha"
      exit 0
    fi
    if [[ "$case" == bad-page ]]; then
      printf '{"workflow_runs":[] }\n{"not":"a page"}\n'
      exit 0
    fi
    event=$(state_get ci.json event); [[ -n "$event" ]] || event=push
    branch=$(state_get ci.json branch); [[ -n "$branch" ]] || branch=dev
    status_value=$(state_get ci.json status); [[ -n "$status_value" ]] || status_value=completed
    conclusion=$(state_get ci.json conclusion); [[ -n "$conclusion" ]] || conclusion=success
    attempt=$(state_get ci.json attempt); [[ -n "$attempt" ]] || attempt=1
    run_number=$(state_get ci.json run_number); [[ -n "$run_number" ]] || run_number=2
    older=$(state_get ci.json older)
    if [[ "$older" == true ]]; then
      printf '{"workflow_runs":[{"id":1,"run_number":1,"run_attempt":1,"head_sha":"%s","head_branch":"dev","event":"push","status":"completed","conclusion":"success"},{"id":2,"run_number":%s,"run_attempt":%s,"head_sha":"%s","head_branch":"%s","event":"%s","status":"%s","conclusion":"%s"}]}\n' "$sha" "$run_number" "$attempt" "$sha" "$branch" "$event" "$status_value" "$conclusion"
    else
      printf '{"workflow_runs":[{"id":2,"run_number":%s,"run_attempt":%s,"head_sha":"%s","head_branch":"%s","event":"%s","status":"%s","conclusion":"%s"}]}\n' "$run_number" "$attempt" "$sha" "$branch" "$event" "$status_value" "$conclusion"
    fi
    exit 0
  fi
  if [[ "$query" == */git/ref/tags/* ]]; then
    case=$(state_get tag.json case)
    if [[ "$case" == error ]]; then printf '%s\n' '{"message":"tag api failed","status":"500"}'; exit 1; fi
    if [[ "$case" == missing ]]; then printf '%s\n' '{"message":"Not Found","status":"404"}'; exit 1; fi
    object_sha=$(state_get tag.json object_sha)
    object_type=$(state_get tag.json object_type)
    [[ -n "$object_type" ]] || object_type=commit
    [[ -n "$object_sha" ]] || object_sha="$RELEASE_SHA"
    printf '{"ref":"refs/tags/%s","object":{"sha":"%s","type":"%s"}}\n' "$RELEASE_TAG" "$object_sha" "$object_type"
    exit 0
  fi
  if [[ "$query" == */git/tags/* ]]; then
    case=$(state_get tag.json peel)
    if [[ "$case" == error ]]; then printf '%s\n' '{"message":"peel failed","status":"500"}'; exit 1; fi
    peeled_sha=$(state_get tag.json peeled_sha)
    peeled_type=$(state_get tag.json peeled_type)
    [[ -n "$peeled_type" ]] || peeled_type=commit
    [[ -n "$peeled_sha" ]] || peeled_sha="$RELEASE_SHA"
    object_sha=$(state_get tag.json object_sha)
    printf '{"sha":"%s","object":{"sha":"%s","type":"%s"}}\n' "$object_sha" "$peeled_sha" "$peeled_type"
    exit 0
  fi
  if [[ "$query" == */releases\?per_page=100 ]]; then
    case=$(state_get release.json case)
    if [[ "$case" == error ]]; then printf '%s\n' '{"message":"release api failed","status":"500"}'; exit 1; fi
    if [[ "$case" == paged-draft ]]; then
      printf '%s\n' '[]'
      printf '[{"draft":true,"target_commitish":"%s","tag_name":"%s"}]\n' "$RELEASE_SHA" "$RELEASE_TAG"
      exit 0
    fi
    if [[ "$case" == missing || "$case" == create-error ]]; then printf '%s\n' '[]'; exit 0; fi
    draft=$(state_get release.json draft); [[ -n "$draft" ]] || draft=false
    target=$(state_get release.json target); [[ -n "$target" ]] || target="$RELEASE_SHA"
    printf '[{"draft":%s,"target_commitish":"%s","tag_name":"%s"}]\n' "$draft" "$target" "$RELEASE_TAG"
    exit 0
  fi
  printf 'unexpected api: %s\n' "$query" >&2
  exit 90
fi
if [[ "${1:-}" == release && "${2:-}" == create ]]; then
  if [[ "$(state_get release.json case)" == create-error ]]; then
    echo 'release create failed' >&2
    exit 1
  fi
  printf '%s\n' "$*" > "$GH_STATE/created.txt"
  exit 0
fi
printf 'unexpected gh call: %s\n' "$*" >&2
exit 90
