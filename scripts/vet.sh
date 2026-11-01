#!/usr/bin/env bash
# Run go vet on the engine's packages for the target in GOOS/GOARCH.
# go vet also reports findings in third_party/buildkit, which it analyses as
# a dependency inside this module; that code is upstream's, kept as
# scripts/sync-buildkit.sh copies it, so its findings are left to upstream.
# Compile errors there still fail the build step.
set -uo pipefail
cd "$(dirname "$0")/.."
pkgs=$(go list ./... | grep -v /third_party/)
out=$(go vet $pkgs 2>&1)
own=$(printf '%s\n' "$out" | grep -v -e '^third_party/buildkit/' -e '^#' -e '^$' || true)
if [ -n "$own" ]; then
  printf '%s\n' "$own"
  exit 1
fi
