#!/usr/bin/env bash
# Uses the isolation pattern documented in docs/CI.md; no repository allowlist.
set -euo pipefail

scan_root=$(git rev-parse --show-toplevel)
scan_git=$(git rev-parse --absolute-git-dir)
if [[ $(git rev-parse --is-shallow-repository) != false ]]; then
  echo 'Secret scan requires full history; fetch --unshallow first.' >&2
  exit 1
fi

# What is scanned follows the event. A pull_request run scans the commits the pull request adds to
# its base: the range from PR_BASE_SHA (the workflow passes the event's base tip) to the merge
# candidate that is checked out, still with merge-parent diffs (-m). Another branch's content then
# cannot fail it. Every other event, and a run outside Actions, scans every fetched ref as before.
# A pull request without a usable base fails here, before anything is downloaded, rather than
# scanning more or less than it says.
scan_log_opts='--all -m'
if [[ ${GITHUB_EVENT_NAME:-} == pull_request ]]; then
  scan_base=${PR_BASE_SHA:-}
  if [[ ! $scan_base =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] ||
    ! scan_base=$(git rev-parse --verify --quiet "${scan_base}^{commit}"); then
    echo 'A pull_request scan needs PR_BASE_SHA: the full SHA of the base commit, present in this checkout.' >&2
    exit 1
  fi
  scan_head=$(git rev-parse --verify HEAD)
  scan_log_opts="-m $scan_base..$scan_head"
  echo "Scanning the commits the pull request adds: $scan_base..$scan_head"
else
  echo 'Scanning every fetched ref.'
fi

scan_temp=$(mktemp -d)
trap 'rm -rf "$scan_temp"' EXIT
unset GITLEAKS_CONFIG GITLEAKS_CONFIG_TOML

scan_version=8.30.1
scan_archive="gitleaks_${scan_version}_linux_x64.tar.gz"
scan_digest=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb
curl --fail --silent --show-error --location --connect-timeout 15 --max-time 120 \
  "https://github.com/gitleaks/gitleaks/releases/download/v${scan_version}/${scan_archive}" \
  --output "$scan_temp/$scan_archive"
printf '%s  %s\n' "$scan_digest" "$scan_temp/$scan_archive" | sha256sum --check --status
tar -xzf "$scan_temp/$scan_archive" -C "$scan_temp" gitleaks
touch "$scan_temp/empty.ignore"

# Gitleaks also discovers ignore files below its target. Use the Git database,
# with a temporary cwd, to keep PR working-tree ignores out of that lookup.
cd "$scan_temp"
./gitleaks git "$scan_git" --config "$scan_root/.gitleaks.toml" \
  --gitleaks-ignore-path "$scan_temp/empty.ignore" --ignore-gitleaks-allow \
  --log-opts="$scan_log_opts" --redact --no-banner
