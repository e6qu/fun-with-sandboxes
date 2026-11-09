#!/usr/bin/env bash
# shard.sh N prints the go test -run pattern of shard N of the end-to-end
# suite: the top-level tests of the files listed for it below, or, for the
# shard after the last listed one, of every file no shard lists, so a new
# file runs without an edit here. The lists balance the shards by the
# files' running times on a macOS runner.
set -euo pipefail
cd "$(dirname "$0")"
shards=(
  "cri_test.go kubernetes_test.go podman_test.go dind_test.go"
  "stock_test.go features_test.go signals_test.go build_test.go"
  "compose_test.go stores_test.go analytics_test.go mounts_test.go"
)
n=${1:?usage: shard.sh N, N from 1 to $((${#shards[@]} + 1))}
listed=" ${shards[*]} "
if ((n >= 1 && n <= ${#shards[@]})); then
  files=${shards[n - 1]}
elif ((n == ${#shards[@]} + 1)); then
  files=$(for f in *_test.go; do [[ $listed == *" $f "* ]] || echo "$f"; done)
else
  echo "no shard $n" >&2
  exit 1
fi
for f in $files; do
  [ -e "$f" ] || {
    echo "shard.sh lists $f, which does not exist" >&2
    exit 1
  }
done
tests=$(sed -n 's/^func \(Test[A-Za-z0-9_]*\)(t \*testing\.T) {$/\1/p' $files | paste -sd'|' -)
[ -n "$tests" ] || {
  echo "shard $n has no tests" >&2
  exit 1
}
echo "^($tests)\$"
