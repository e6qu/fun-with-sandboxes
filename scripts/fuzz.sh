#!/usr/bin/env bash
# Run every fuzz target of the module's own packages for $1 each (default
# 30s), one after another: go test fuzzes one target of one package at a
# time. A failure leaves its input under the package's testdata/fuzz, which
# then runs as a test case of its own.
#
# go test now and then fails a target with "context deadline exceeded" as
# its time runs out, having found nothing (golang/go#48591): a run that
# fails so, and wrote no input, counts as passed.
set -euo pipefail
cd "$(dirname "$0")/.."
time=${1:-30s}
grep -rl --include='*_test.go' '^func Fuzz' internal cmd images | sort | while read -r f; do
  pkg=./$(dirname "$f")
  for target in $(sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' "$f"); do
    echo "== $pkg $target"
    log=$(mktemp)
    if ! go test -run '^$' -fuzz "^$target\$" -fuzztime "$time" "$pkg" 2>&1 | tee "$log"; then
      if grep -q 'Failing input written to' "$log" || ! grep -q 'context deadline exceeded' "$log"; then
        rm -f "$log"
        exit 1
      fi
      echo "== $pkg $target: the deadline passed mid-run; nothing found"
    fi
    rm -f "$log"
  done
done
