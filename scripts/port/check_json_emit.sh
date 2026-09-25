#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
if find internal/relay -name '*.go' \( -path '*/cli/*' -o -name 'cmd_*.go' \) -exec grep -n -E 'json\.Marshal\(|json\.NewEncoder\(' {} +; then
  echo 'JSON emission outside internal/contract' >&2
  exit 1
fi
printf 'JSON emission restricted to internal/contract\n'
