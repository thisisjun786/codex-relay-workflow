#!/usr/bin/env bash
set -euo pipefail
# The release workflow tests' fake git (internal/contracttest/release.go), taken from the Python
# tests' fake_git.sh. The one difference: the push state is one file per key,
# $GH_STATE/push/<key>, read with cat, so no JSON parser (and no Python) is needed.
push_state() {
  local path="$GH_STATE/push/$1"
  if [[ -f "$path" ]]; then cat "$path"; fi
}
real="${GIT_REAL:?}"
if [[ "${1:-}" == push && "${2:-}" == origin && "${3:-}" == *:refs/heads/main ]]; then
  action=ok
  if [[ "$(push_state lie_main)" == true ]]; then action=lie; fi
  if [[ "$(push_state fail_main)" == true ]]; then action=fail; fi
  if [[ "$action" == fail ]]; then
    echo 'remote rejected main update' >&2
    exit 1
  fi
  if [[ "$action" == lie ]]; then
    "$real" "$@"
    # Report a different commit after a push that did update the ref.
    exit 0
  fi
fi
if [[ "${1:-}" == ls-remote && "${2:-}" == origin && "${3:-}" == refs/heads/main ]]; then
  action=ok
  if [[ "$(push_state lie_main)" == true ]]; then action=lie; fi
  if [[ "$action" == lie ]]; then
    printf '%s\trefs/heads/main\n' 0000000000000000000000000000000000000000
    exit 0
  fi
fi
exec "$real" "$@"
