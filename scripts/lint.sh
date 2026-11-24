#!/usr/bin/env bash
# Static checks beyond go vet and staticcheck, none of which costs the
# engine anything at run time, for the target in GOOS/GOARCH:
#   ineffassign  assignments nothing reads
#   nilness      nil dereferences and comparisons the control flow decides
#   exhaustive   switches over the module's enums that miss a value (a
#                default case counts; //exhaustive:ignore with a reason
#                marks a switch partial by design)
#   ShellCheck   the scripts
#   actionlint   the workflows, and ShellCheck over their run steps
# Tools install into $GOBIN (or go env GOPATH/bin) at pinned versions;
# ShellCheck comes from the system's packages.
set -euo pipefail
cd "$(dirname "$0")/.." || exit
bin=${GOBIN:-$(go env GOPATH)/bin}
GOOS='' GOARCH='' GOBIN=$bin go install github.com/gordonklaus/ineffassign@v0.2.0
GOOS='' GOARCH='' GOBIN=$bin go install golang.org/x/tools/go/analysis/passes/nilness/cmd/nilness@v0.53.0
GOOS='' GOARCH='' GOBIN=$bin go install github.com/nishanths/exhaustive/cmd/exhaustive@v0.17.1
GOOS='' GOARCH='' GOBIN=$bin go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
pkgs=()
while read -r p; do pkgs+=("$p"); done < <(go list ./... | grep -v /third_party/)
status=0
# Each reports third_party/buildkit's findings too, as it analyses it as a
# dependency; that code is upstream's (scripts/vet.sh).
own() { grep -v -e '/third_party/buildkit/' -e '^third_party/buildkit/' || true; }
for check in ineffassign nilness exhaustive; do
  flags=()
  [ "$check" = exhaustive ] && flags=(-d)
  # A tool exits non-zero on findings, which own() then sifts.
  out=$({ "$bin/$check" ${flags[@]+"${flags[@]}"} "${pkgs[@]}" 2>&1 || true; } | own)
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
    status=1
  fi
done
(cd tests/e2e && GOFLAGS=-tags=e2e "$bin"/exhaustive -d ./...) || status=1
shellcheck scripts/*.sh || status=1
"$bin"/actionlint || status=1
exit $status
