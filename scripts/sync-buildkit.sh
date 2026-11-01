#!/usr/bin/env bash
# Copy the BuildKit packages the engine uses into third_party/buildkit,
# rewrite their imports into this module, and apply the patches in
# third_party/buildkit-patches. Run it after changing the BuildKit version
# or the packages internal/buildkit imports.
#
# The packages live in this module, not behind a replace directive, so
# `go install github.com/e6qu/fun-with-sandboxes/cmd/fwsd@<version>` keeps
# working.
set -euo pipefail

version=${BUILDKIT_VERSION:-v0.34.1}
here=$(cd "$(dirname "$0")/.." && pwd)
module=github.com/e6qu/fun-with-sandboxes
upstream=github.com/moby/buildkit
dest="$here/third_party/buildkit"
cd "$here"

# Resolve the package set from a scratch module that imports, from upstream,
# each BuildKit package internal/buildkit imports.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
src=$(GOFLAGS=-mod=mod go mod download -json "$upstream@$version" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')
imports=$(grep -ho "\"\($module/third_party/buildkit\|$upstream\)[^\"]*\"" internal/buildkit/*.go \
  | tr -d '"' | sed "s|^$module/third_party/buildkit|$upstream|" | sort -u)
mkdir -p "$work/probe"
{
  echo "package probe"
  echo "import ("
  for i in $imports; do echo "	_ \"$i\""; done
  echo ")"
} > "$work/probe/probe.go"
printf 'module probe\n\ngo %s\n' "$(go env GOVERSION | sed 's/^go//')" > "$work/go.mod"
(
  cd "$work"
  GOFLAGS=-mod=mod go get "$upstream@$version" >/dev/null 2>&1
  GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
  # go mod tidy resolves imports for every platform, so the copied set
  # covers the platforms BuildKit builds for, not only the ones we run on.
  for goos in darwin linux windows freebsd; do
    for goarch in arm64 amd64; do
      GOOS=$goos GOARCH=$goarch GOFLAGS=-mod=mod go list -deps ./probe
    done
  done | grep "^$upstream\(/\|$\)" | sort -u
) > "$work/packages"
[ -s "$work/packages" ] || { echo "found no BuildKit packages to copy" >&2; exit 1; }

rm -rf "$dest"
mkdir -p "$dest"
while read -r pkg; do
  rel=${pkg#"$upstream"}
  rel=${rel#/}
  mkdir -p "$dest/$rel"
  find "$src/$rel" -maxdepth 1 -type f \( -name '*.go' -o -name '*.s' \) ! -name '*_test.go' \
    -exec cp {} "$dest/$rel/" \;
done < "$work/packages"
cp "$src/LICENSE" "$dest/LICENSE"
[ -f "$src/NOTICE" ] && cp "$src/NOTICE" "$dest/NOTICE"
chmod -R u+w "$dest"

# Rewrite import declarations only. Generated protobuf code embeds its
# upstream Go package path inside serialized descriptors, which must stay as
# they are.
rewrite_imports() {
  awk -v from="\"$upstream" -v to="\"$module/third_party/buildkit" '
    function fix(line) {
      if (index(line, from "/") || index(line, from "\"")) sub(from, to, line)
      return line
    }
    /^import \($/ { block = 1; print; next }
    block && /^\)$/ { block = 0; print; next }
    block { print fix($0); next }
    /^import ([A-Za-z_.]+ )?"/ { print fix($0); next }
    { print }
  ' "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}
find "$dest" -name '*.go' | while read -r f; do rewrite_imports "$f"; done

for p in third_party/buildkit-patches/*.patch; do
  patch -s -d "$dest" -p1 < "$p"
done
gofmt -w "$dest"

cat > "$dest/README.md" <<README
# BuildKit $version (vendored subset)

These are the packages of [BuildKit](https://github.com/moby/buildkit)
$version that \`internal/buildkit\` uses, copied by
\`scripts/sync-buildkit.sh\` under BuildKit's Apache License 2.0 (see
\`LICENSE\` and \`NOTICE\`).

Changes from upstream:

- Import paths point into this module.
- The patches in \`third_party/buildkit-patches\` are applied:
$(for p in third_party/buildkit-patches/*.patch; do echo "  - \`$(basename "$p")\`"; done)
README
echo "copied $(wc -l < "$work/packages" | tr -d ' ') packages of $upstream@$version"
