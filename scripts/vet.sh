#!/usr/bin/env bash
# Run go vet on the engine's packages for the target in GOOS/GOARCH.
# go vet also reports findings in third_party/buildkit, which it analyses as
# a dependency inside this module; that code is kept byte for byte as
# scripts/sync-buildkit.sh copies it and is never edited here, so its
# findings are not gated on. Compile errors there still fail the build step.
set -uo pipefail
cd "$(dirname "$0")/.." || exit
pkgs=()
while read -r p; do pkgs+=("$p"); done < <(go list ./... | grep -v /third_party/)
out=$(go vet "${pkgs[@]}" 2>&1)
own=$(printf '%s\n' "$out" | grep -v -e '^third_party/buildkit/' -e '^#' -e '^$' || true)
if [ -n "$own" ]; then
  printf '%s\n' "$own"
  exit 1
fi
