#!/usr/bin/env bash
# Build the darwin image archives of the catalog into $1.
# Needs macOS (install_name_tool, codesign) and Nix.
set -euo pipefail

out=${1:?usage: build-images.sh OUTPUT_DIR}
# nixos-26.05, pinned, so a rebuild makes the same images.
nixpkgs=${NIXPKGS:-github:NixOS/nixpkgs/9bb52223f4f6a0cc606fd0852d8784f896c137d5}
here=$(cd "$(dirname "$0")/.." && pwd)
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
mkdir -p "$out"
cd "$here"

go build -o "$build/fws-mkimage" ./cmd/fws-mkimage
go build -o "$build/postgres-entry" ./images/postgres/entry
go build -o "$build/redis-entry" ./images/redis/entry
go build -o "$build/nginx-entry" ./images/nginx/entry
go build -o "$build/mariadb-entry" ./images/mariadb/entry
go build -o "$build/gitea-entry" ./images/gitea/entry
go build -o "$build/dynamodb-entry" ./images/dynamodb/entry
go build -o "$build/toolbox" ./images/toolbox

# pkg_with ATTR BINARY prints the output of ATTR that holds bin/BINARY.
# Unfree packages, such as DynamoDB Local, build too.
pkg_with() {
  local p
  for p in $(NIXPKGS_ALLOW_UNFREE=1 nix --extra-experimental-features 'nix-command flakes' build --impure --no-link --print-out-paths "$nixpkgs#$1"); do
    if [ -x "$p/bin/$2" ]; then
      echo "$p"
      return
    fi
  done
  echo "no output of $1 holds bin/$2" >&2
  exit 1
}

for v in 16 18; do
  pg=$(pkg_with "postgresql_$v" postgres)
  nix-store -qR "$pg" > "$build/pg$v.closure"
  "$build/fws-mkimage" -spec images/postgres/image.json -tag "fws/postgres:$v" -closure "$build/pg$v.closure" -pkg "$pg" \
    -file "/entry=$build/postgres-entry" -out "$out/postgres-$v.tar"
done

redis=$(pkg_with redis redis-server)
nix-store -qR "$redis" > "$build/redis.closure"
"$build/fws-mkimage" -spec images/redis/image.json -closure "$build/redis.closure" -pkg "$redis" \
  -file "/entry=$build/redis-entry" -out "$out/redis.tar"

nginx=$(pkg_with nginx nginx)
nix-store -qR "$nginx" > "$build/nginx.closure"
"$build/fws-mkimage" -spec images/nginx/image.json -closure "$build/nginx.closure" -pkg "$nginx" \
  -file "/entry=$build/nginx-entry" -file "/usr/share/nginx/html/index.html=$nginx/html/index.html" -out "$out/nginx.tar"

mariadb=$(pkg_with mariadb mariadbd)
nix-store -qR "$mariadb" > "$build/mariadb.closure"
"$build/fws-mkimage" -spec images/mariadb/image.json -closure "$build/mariadb.closure" -pkg "$mariadb" \
  -file "/entry=$build/mariadb-entry" -out "$out/mariadb.tar"

python=$(pkg_with python314 python3.14)
nix-store -qR "$python" > "$build/python.closure"
"$build/fws-mkimage" -spec images/python/image.json -closure "$build/python.closure" -pkg "$python" -out "$out/python.tar"

node=$(pkg_with nodejs_26 node)
nix-store -qR "$node" > "$build/node.closure"
"$build/fws-mkimage" -spec images/node/image.json -closure "$build/node.closure" -pkg "$node" -out "$out/node.tar"

bun=$(pkg_with bun bun)
nix-store -qR "$bun" > "$build/bun.closure"
"$build/fws-mkimage" -spec images/bun/image.json -closure "$build/bun.closure" -pkg "$bun" -out "$out/bun.tar"

gitea=$(pkg_with gitea gitea)
gitea_data=$(NIXPKGS_ALLOW_UNFREE=1 nix --extra-experimental-features 'nix-command flakes' build --impure --no-link --print-out-paths "$nixpkgs#gitea.data")
git=$(pkg_with git git)
nix-store -qR "$gitea" "$gitea_data" "$git" | sort -u > "$build/gitea.closure"
"$build/fws-mkimage" -spec images/gitea/image.json -closure "$build/gitea.closure" -pkg "$gitea" -pkg "$git" \
  -env "GITEA_DATA=$gitea_data" -file "/entry=$build/gitea-entry" -out "$out/gitea.tar"

dynamodb=$(pkg_with dynamodb-local dynamodb-local)
# The Java runtime nixpkgs packages DynamoDB Local with.
jre=
for p in $(nix-store -qR "$dynamodb"); do
  if [ -x "$p/bin/java" ]; then jre=$p; fi
done
[ -n "$jre" ] || { echo "DynamoDB Local's closure holds no Java runtime" >&2; exit 1; }
jdk=$(pkg_with jdk_headless javac)
mkdir -p "$build/dynamodb-launcher"
"$jdk/bin/javac" --release 17 -d "$build/dynamodb-launcher" images/dynamodb/launcher/Launcher.java
nix-store -qR "$dynamodb" > "$build/dynamodb.closure"
"$build/fws-mkimage" -spec images/dynamodb/image.json -closure "$build/dynamodb.closure" -pkg "$jre" \
  -env "DYNAMODB_HOME=$dynamodb/share/dynamodb-local" -file "/entry=$build/dynamodb-entry" \
  -file "/opt/fws=$build/dynamodb-launcher" -out "$out/dynamodb.tar"

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
