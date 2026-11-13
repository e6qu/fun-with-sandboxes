#!/usr/bin/env bash
# e2e-shard.sh SHARDS SHARD prints a go test -run pattern that selects the
# end-to-end tests of shard SHARD (1 to SHARDS). Tests are spread by their
# duration in an earlier run (tests/e2e/durations.txt, seconds per test),
# longest first, each to the shard with the least time so far; a test the
# file does not list counts as 30 seconds. Every test lands in exactly one
# shard, so the shards together run the whole suite.
set -euo pipefail
shards=${1:?usage: e2e-shard.sh SHARDS SHARD}
shard=${2:?usage: e2e-shard.sh SHARDS SHARD}
here=$(cd "$(dirname "$0")/.." && pwd)
grep -h '^func Test' "$here"/tests/e2e/*_test.go | grep -v '^func TestMain(' |
  sed 's/^func \(Test[A-Za-z0-9_]*\).*/\1/' | sort -u |
  awk -v shards="$shards" -v shard="$shard" -v durations="$here/tests/e2e/durations.txt" '
    BEGIN { while ((getline line < durations) > 0) { split(line, f, " "); d[f[1]] = f[2] } }
    { t[NR] = $1; w[NR] = ($1 in d) ? d[$1] : 30 }
    END {
      n = NR
      # Longest first; ties by name, so every shard computes the same split.
      for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++)
        if (w[j] > w[i] || (w[j] == w[i] && t[j] < t[i])) { x = w[i]; w[i] = w[j]; w[j] = x; y = t[i]; t[i] = t[j]; t[j] = y }
      for (s = 1; s <= shards; s++) load[s] = 0
      sep = ""
      for (i = 1; i <= n; i++) {
        best = 1
        for (s = 2; s <= shards; s++) if (load[s] < load[best]) best = s
        load[best] += w[i]
        if (best == shard) { out = out sep t[i]; sep = "|" }
      }
      printf "^(%s)$\n", out
      printf "shard %d of %d: about %d s\n", shard, shards, load[shard] > "/dev/stderr"
    }'
