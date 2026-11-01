#!/usr/bin/env bash
# Build the darwin image archives (postgres, redis, toolbox, ryuk) into $1.
# Needs macOS (install_name_tool, codesign) and Nix.
set -euo pipefail

out=${1:?usage: build-images.sh OUTPUT_DIR}
nixpkgs=${NIXPKGS:-github:NixOS/nixpkgs/nixos-25.05}
here=$(cd "$(dirname "$0")/.." && pwd)
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
mkdir -p "$out"
cd "$here"

go build -o "$build/fws-mkimage" ./cmd/fws-mkimage
go build -o "$build/postgres-entry" ./images/postgres/entry
go build -o "$build/redis-entry" ./images/redis/entry
go build -o "$build/toolbox" ./images/toolbox

# pkg_with ATTR BINARY prints the output of ATTR that holds bin/BINARY.
pkg_with() {
  local p
  for p in $(nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths "$nixpkgs#$1"); do
    if [ -x "$p/bin/$2" ]; then
      echo "$p"
      return
    fi
  done
  echo "no output of $1 holds bin/$2" >&2
  exit 1
}

pg=$(pkg_with postgresql_16 postgres)
nix-store -qR "$pg" > "$build/pg.closure"
"$build/fws-mkimage" -spec images/postgres/image.json -closure "$build/pg.closure" -pkg "$pg" \
  -file "/entry=$build/postgres-entry" -out "$out/postgres.tar"

redis=$(pkg_with redis redis-server)
nix-store -qR "$redis" > "$build/redis.closure"
"$build/fws-mkimage" -spec images/redis/image.json -closure "$build/redis.closure" -pkg "$redis" \
  -file "/entry=$build/redis-entry" -out "$out/redis.tar"

"$build/fws-mkimage" -spec images/toolbox/image.json -file "/toolbox=$build/toolbox" -out "$out/toolbox.tar"

# Ryuk, Testcontainers' reaper, built from its 0.14.0 release with
# images/ryuk/listen-loopback.patch, so it listens on loopback as the
# container contract asks.
ryuk_commit=b3726afd6cc2c36628abcc08e9cabac43f587384
ryuk_src=$(GOFLAGS=-mod=mod go mod download -json "github.com/testcontainers/moby-ryuk@$ryuk_commit" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')
cp -R "$ryuk_src" "$build/ryuk-src"
chmod -R u+w "$build/ryuk-src"
patch -s -d "$build/ryuk-src" -p1 < images/ryuk/listen-loopback.patch
(cd "$build/ryuk-src" && GOFLAGS=-mod=mod go build -o "$build/ryuk" .)
go build -o "$build/ryuk-entry" ./images/ryuk/entry
go build -o "$build/ryuk-nc" ./images/ryuk/nc
"$build/fws-mkimage" -spec images/ryuk/image.json -file "/entry=$build/ryuk-entry" -file "/ryuk=$build/ryuk" \
  -file "/bin/nc=$build/ryuk-nc" -out "$out/ryuk.tar"

ls -l "$out"
