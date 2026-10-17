#!/usr/bin/env bash
set -eo pipefail

if [[ $# -gt 1 ]]; then
  echo 'usage: lint-changed.sh [base]' >&2
  exit 2
fi
TMPBASE="${TMPDIR:-/tmp}"
case "$TMPBASE" in
  /*) ;;
  *) printf 'tmpdir_not_absolute: TMPDIR=%s; set TMPDIR to an absolute directory\n' "$TMPBASE" >&2; exit 2 ;;
esac
base=${1:-origin/dev}
root=$(git rev-parse --show-toplevel)
cd "$root"
if ! merge_base=$(git merge-base "$base" HEAD); then
  echo "lint-changed: no merge-base with $base" >&2
  exit 2
fi
go=${GO:-go}
changed=$(mktemp "$TMPBASE/crw-lint-changed.XXXXXX")
trap 'rm -f "$changed"' EXIT
git diff --name-only --no-renames -z "$merge_base" HEAD -- '*.go' > "$changed"
git diff --name-only --no-renames -z --cached -- '*.go' >> "$changed"
git diff --name-only --no-renames -z -- '*.go' >> "$changed"
git ls-files --others --exclude-standard -z -- '*.go' >> "$changed"
files=()
dirs=()
while IFS= read -r -d '' file; do
  dir=.
  [[ "$file" != */* ]] || dir=${file%/*}
  dir=./$dir
  seen=false
  for item in "${dirs[@]}"; do
    [[ "$item" != "$dir" ]] || seen=true
  done
  $seen || dirs+=("$dir")
  [[ -f "$file" ]] || continue
  seen=false
  for item in "${files[@]}"; do
    [[ "$item" != "$file" ]] || seen=true
  done
  $seen || files+=("$file")
done < "$changed"
if [[ ${#dirs[@]} -eq 0 ]]; then
  echo "lint-changed: no Go files changed against $base"
  exit 0
fi
dev=()
integration=()
for dir in "${dirs[@]}"; do
  case "$dir" in ./cmd/crw-dev|./cmd/crw-dev/*|./internal/dev|./internal/dev/*) dev+=("$dir");; esac
  case "$dir" in ./internal/runtime/integration|./internal/runtime/integration/*) integration+=("$dir");; esac
done
status=0
step() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if ! "$@"; then
    status=1
  fi
}
run_scope() {
  local tag=$1
  shift
  [[ $# -gt 0 ]] || return 0
  local output pkg
  local tag_args=() packages=()
  [[ -z "$tag" ]] || tag_args=(-tags "$tag")
  output=$("$go" list -e "${tag_args[@]}" -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles .InvalidGoFiles}}{{.ImportPath}}{{end}}' "$@") || return 1
  while IFS= read -r pkg; do
    [[ -z "$pkg" ]] || packages+=("$pkg")
  done <<< "$output"
  [[ ${#packages[@]} -gt 0 ]] || return 0
  step "$go" vet "${tag_args[@]}" "${packages[@]}"
  if [[ "$tag" != integration ]]; then
    step "$go" run honnef.co/go/tools/cmd/staticcheck "${tag_args[@]}" "${packages[@]}"
  fi
}
run_scope '' "${dirs[@]}"
run_scope dev "${dev[@]}"
run_scope integration "${integration[@]}"
if [[ ${#files[@]} -gt 0 ]]; then
  if ! unformatted=$(gofmt -l "${files[@]}"); then
    status=1
  fi
  if [[ -n "$unformatted" ]]; then
    printf 'gofmt needed:\n%s\n' "$unformatted"
    status=1
  fi
fi
printf 'lint-changed: %s files, %s directories, exit %s\n' "${#files[@]}" "${#dirs[@]}" "$status"
exit "$status"
