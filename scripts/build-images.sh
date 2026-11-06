#!/usr/bin/env bash
# Build the darwin image archives of the catalog into $1.
# Needs macOS (install_name_tool, codesign) and Nix.
#
# build-images.sh --server-derivations [NAME...] prints the derivations of
# the database servers Nix builds from source (images/servers.nix), and
# build-images.sh --servers [NAME...] builds them and prints their outputs,
# so CI can keep them between runs.
set -euo pipefail

# nixos-26.05, pinned, so a rebuild makes the same images.
nixpkgs=${NIXPKGS:-github:NixOS/nixpkgs/9bb52223f4f6a0cc606fd0852d8784f896c137d5}
here=$(cd "$(dirname "$0")/.." && pwd)
nix=(nix --extra-experimental-features 'nix-command flakes')
servers=(--impure -f "$here/images/servers.nix" --argstr nixpkgs "$nixpkgs")
case "${1:-}" in
--server-derivations | --servers)
  mode=$1
  shift
  [ $# -gt 0 ] || set -- mysql mariadb
  if [ "$mode" = --servers ]; then
    "${nix[@]}" build --no-link --print-out-paths "${servers[@]}" "$@"
  else
    "${nix[@]}" path-info --derivation "${servers[@]}" "$@"
  fi
  exit
  ;;
esac
out=${1:?usage: build-images.sh OUTPUT_DIR | --server-derivations | --servers}
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
go build -o "$build/lambda-entrypoint" ./images/lambda/entry
go build -o "$build/rabbitmq-entry" ./images/rabbitmq/entry
go build -o "$build/playwright-entry" ./images/playwright/entry
go build -o "$build/toolbox" ./images/toolbox
go build -o "$build/mysql-entry" ./images/mysql/entry
go build -o "$build/valkey-entry" ./images/valkey/entry
go build -o "$build/seaweedfs-entry" ./images/seaweedfs/entry
go build -o "$build/memcached-entry" ./images/memcached/entry
go build -o "$build/nc" ./images/nc

# output_with BINARY INSTALLABLE... builds INSTALLABLE and prints its
# output that holds bin/BINARY. Unfree packages, such as DynamoDB Local,
# build too.
output_with() {
  local bin=$1 p
  shift
  for p in $(NIXPKGS_ALLOW_UNFREE=1 "${nix[@]}" build --impure --no-link --print-out-paths "$@"); do
    if [ -x "$p/bin/$bin" ]; then
      echo "$p"
      return
    fi
  done
  echo "no output of $* holds bin/$bin" >&2
  exit 1
}

# pkg_with ATTR BINARY prints the output of nixpkgs' ATTR that holds
# bin/BINARY; server_with ATTR BINARY that of images/servers.nix's ATTR.
pkg_with() { output_with "$2" "$nixpkgs#$1"; }
server_with() { output_with "$2" "${servers[@]}" "$1"; }

# Every catalog image carries a shell of its own at /bin/sh: the loader
# drops the container library from Apple's shells, and with it from what
# system(), popen() and package managers run through them.
dash=$(pkg_with dash dash)
# The network databases a C library reads, which Debian-based official
# images carry in netbase.
iana=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#iana-etc")
closure() { nix-store -qR "$@" "$dash" | sort -u; }
# /bin/nc answers the in-container port check Testcontainers runs, which
# reads /proc/net/tcp on Linux.
mkimage() {
  "$build/fws-mkimage" "$@" -pkg "$dash" -link /bin/sh=dash -file "/bin/nc=$build/nc" \
    -file "/etc/services=$iana/etc/services" -file "/etc/protocols=$iana/etc/protocols"
}

# accounts NAME [USER UID GID HOME]... writes the /etc/passwd and
# /etc/group of image NAME, which hold root, nobody and the users the
# official image has, and prints the -file arguments that add them.
accounts() {
  local d="$build/etc-$1"
  shift
  mkdir -p "$d"
  printf 'root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/bin/false\n' > "$d/passwd"
  printf 'root:x:0:\nnobody:x:65534:\n' > "$d/group"
  while [ $# -ge 4 ]; do
    printf '%s:x:%s:%s::%s:/bin/sh\n' "$1" "$2" "$3" "$4" >> "$d/passwd"
    printf '%s:x:%s:\n' "$1" "$3" >> "$d/group"
    shift 4
  done
  echo "-file /etc/passwd=$d/passwd -file /etc/group=$d/group"
}

for v in 16 18; do
  pg=$(pkg_with "postgresql_$v" postgres)
  closure "$pg" > "$build/pg$v.closure"
  mkimage $(accounts postgres postgres 999 999 /var/lib/postgresql) -spec images/postgres/image.json -tag "fws/postgres:$v" -closure "$build/pg$v.closure" -pkg "$pg" \
    -file "/entry=$build/postgres-entry" -out "$out/postgres-$v.tar"
done

redis=$(pkg_with redis redis-server)
closure "$redis" > "$build/redis.closure"
mkimage $(accounts redis redis 999 999 /data) -spec images/redis/image.json -closure "$build/redis.closure" -pkg "$redis" \
  -file "/entry=$build/redis-entry" -out "$out/redis.tar"

nginx=$(pkg_with nginx nginx)
closure "$nginx" > "$build/nginx.closure"
mkimage $(accounts nginx nginx 101 101 /nonexistent) -spec images/nginx/image.json -closure "$build/nginx.closure" -pkg "$nginx" \
  -file "/entry=$build/nginx-entry" -file "/usr/share/nginx/html/index.html=$nginx/html/index.html" -out "$out/nginx.tar"

mariadb=$(server_with mariadb mariadbd)
closure "$mariadb" > "$build/mariadb.closure"
mkimage $(accounts mariadb mysql 999 999 /var/lib/mysql) -spec images/mariadb/image.json -closure "$build/mariadb.closure" -pkg "$mariadb" \
  -file "/entry=$build/mariadb-entry" -out "$out/mariadb.tar"

mysql=$(server_with mysql mysqld)
closure "$mysql" > "$build/mysql.closure"
mkimage $(accounts mysql mysql 999 999 /var/lib/mysql) -spec images/mysql/image.json -closure "$build/mysql.closure" -pkg "$mysql" \
  -file "/entry=$build/mysql-entry" -file "/etc/my.cnf=images/mysql/my.cnf" -out "$out/mysql.tar"

valkey=$(pkg_with valkey valkey-server)
closure "$valkey" > "$build/valkey.closure"
mkimage $(accounts valkey valkey 999 999 /data) -spec images/valkey/image.json -closure "$build/valkey.closure" -pkg "$valkey" \
  -file "/entry=$build/valkey-entry" -out "$out/valkey.tar"

memcached=$(pkg_with memcached memcached)
closure "$memcached" > "$build/memcached.closure"
mkimage $(accounts memcached memcache 11211 11211 /home/memcache) -spec images/memcached/image.json -closure "$build/memcached.closure" -pkg "$memcached" \
  -file "/entry=$build/memcached-entry" -out "$out/memcached.tar"

seaweedfs=$(pkg_with seaweedfs weed)
closure "$seaweedfs" > "$build/seaweedfs.closure"
mkimage $(accounts seaweedfs seaweed 1000 1000 /home/seaweed) -spec images/seaweedfs/image.json -closure "$build/seaweedfs.closure" -pkg "$seaweedfs" \
  -link /usr/bin/weed=../../bin/weed -file "/etc/seaweedfs/filer.toml=images/seaweedfs/filer.toml" -file "/entry=$build/seaweedfs-entry" \
  -out "$out/seaweedfs.tar"

nats=$(pkg_with nats-server nats-server)
closure "$nats" > "$build/nats.closure"
mkimage $(accounts nats) -spec images/nats/image.json -closure "$build/nats.closure" -pkg "$nats" \
  -file "/nats-server.conf=images/nats/nats-server.conf" -out "$out/nats.tar"

python=$(pkg_with python314 python3.14)
closure "$python" > "$build/python.closure"
# nixpkgs marks its Python as managed by Nix, which keeps pip from installing
# into it; the official image's Python takes packages.
python_marker=$(cd "$python" && ls lib/python3.14/EXTERNALLY-MANAGED)
mkimage $(accounts python) -spec images/python/image.json -closure "$build/python.closure" -pkg "$python" \
  -remove "$python/$python_marker" -out "$out/python.tar"

node=$(pkg_with nodejs_26 node)
closure "$node" > "$build/node.closure"
mkimage $(accounts node node 1000 1000 /home/node) -spec images/node/image.json -closure "$build/node.closure" -pkg "$node" -out "$out/node.tar"

bun=$(pkg_with bun bun)
closure "$bun" > "$build/bun.closure"
mkimage $(accounts bun bun 1000 1000 /home/bun) -spec images/bun/image.json -closure "$build/bun.closure" -pkg "$bun" -out "$out/bun.tar"

gitea=$(pkg_with gitea gitea)
gitea_data=$(NIXPKGS_ALLOW_UNFREE=1 "${nix[@]}" build --impure --no-link --print-out-paths "$nixpkgs#gitea.data")
git=$(pkg_with git git)
# Gitea's repository hooks are bash scripts, as in the official image.
bash=$(pkg_with bash bash)
closure "$gitea" "$gitea_data" "$git" "$bash" > "$build/gitea.closure"
mkimage $(accounts gitea git 1000 1000 /data/git) -spec images/gitea/image.json -closure "$build/gitea.closure" -pkg "$gitea" -pkg "$git" -pkg "$bash" \
  -env "GITEA_DATA=$gitea_data" -file "/entry=$build/gitea-entry" -out "$out/gitea.tar"

dynamodb=$(pkg_with dynamodb-local dynamodb-local)
# The Java runtime nixpkgs packages DynamoDB Local with.
jre=
for p in $(nix-store -qR "$dynamodb"); do
  if [ -x "$p/bin/java" ]; then jre=$p; fi
done
[ -n "$jre" ] || { echo "DynamoDB Local's closure holds no Java runtime" >&2; exit 1; }
closure "$dynamodb" > "$build/dynamodb.closure"
mkimage $(accounts dynamodb dynamodblocal 1000 1000 /home/dynamodblocal) -spec images/dynamodb/image.json -closure "$build/dynamodb.closure" -pkg "$jre" \
  -env "DYNAMODB_HOME=$dynamodb/share/dynamodb-local" -file "/entry=$build/dynamodb-entry" -out "$out/dynamodb.tar"

rabbitmq=$(pkg_with rabbitmq-server rabbitmq-server)
# RabbitMQ's scripts run the POSIX tools from PATH.
coreutils=$(pkg_with coreutils dirname)
gnused=$(pkg_with gnused sed)
gnugrep=$(pkg_with gnugrep grep)
gawk=$(pkg_with gawk awk)
closure "$rabbitmq" "$coreutils" "$gnused" "$gnugrep" "$gawk" > "$build/rabbitmq.closure"
mkimage $(accounts rabbitmq rabbitmq 999 999 /var/lib/rabbitmq) -spec images/rabbitmq/image.json -closure "$build/rabbitmq.closure" -pkg "$rabbitmq" \
  -pkg "$coreutils" -pkg "$gnused" -pkg "$gnugrep" -pkg "$gawk" \
  -file "/entry=$build/rabbitmq-entry" -out "$out/rabbitmq.tar"

# The Lambda images: AWS's Runtime Interface Emulator, an interpreter, and
# the image's client of the Lambda Runtime API.
rie=$(pkg_with aws-lambda-rie aws-lambda-rie)

closure "$python" "$rie" > "$build/lambda-python.closure"
mkimage $(accounts lambda-python sbx_user1051 993 990 /home/sbx_user1051) -spec images/lambda/python/image.json -closure "$build/lambda-python.closure" -pkg "$python" -pkg "$rie" \
  -remove "$python/$python_marker" \
  -file "/lambda-entrypoint=$build/lambda-entrypoint" -file "/var/runtime/bootstrap.py=images/lambda/python/bootstrap.py" -file "/var/runtime/bootstrap=images/lambda/python/bootstrap" \
  -out "$out/lambda-python.tar"

node24=$(pkg_with nodejs_24 node)
closure "$node24" "$rie" > "$build/lambda-nodejs.closure"
mkimage $(accounts lambda-nodejs sbx_user1051 993 990 /home/sbx_user1051) -spec images/lambda/nodejs/image.json -closure "$build/lambda-nodejs.closure" -pkg "$node24" -pkg "$rie" \
  -file "/lambda-entrypoint=$build/lambda-entrypoint" -file "/var/runtime/bootstrap.mjs=images/lambda/nodejs/bootstrap.mjs" -file "/var/runtime/bootstrap=images/lambda/nodejs/bootstrap" \
  -out "$out/lambda-nodejs.tar"

# The Java runtime's libraries, from Maven Central, checked against their
# published digests.
jdk17=$(pkg_with jdk17_headless javac)
lib="$build/lambda-java/lib"
mkdir -p "$lib"
while read -r sum path; do
  curl -fsSL --retry 5 -o "$lib/$(basename "$path")" "https://repo1.maven.org/maven2/$path"
  echo "$sum  $lib/$(basename "$path")" | shasum -a 256 -c - >/dev/null
done <<'JARS'
59e36aa26029fbb8f542b023fff86422a5577105764d31839b56d495d73faa5c com/amazonaws/aws-lambda-java-core/1.2.3/aws-lambda-java-core-1.2.3.jar
c04993f33c0f845342653784f14f38373d005280e6359db5f808701cfae73c0c com/fasterxml/jackson/core/jackson-databind/2.17.2/jackson-databind-2.17.2.jar
721a189241dab0525d9e858e5cb604d3ecc0ede081e2de77d6f34fa5779a5b46 com/fasterxml/jackson/core/jackson-core/2.17.2/jackson-core-2.17.2.jar
873a606e23507969f9bbbea939d5e19274a88775ea5a169ba7e2d795aa5156e1 com/fasterxml/jackson/core/jackson-annotations/2.17.2/jackson-annotations-2.17.2.jar
JARS
mkdir -p "$build/lambda-java/classes"
"$jdk17/bin/javac" --release 17 -cp "$lib/*" -d "$build/lambda-java/classes" images/lambda/java/src/fws/lambda/Bootstrap.java
"$jdk17/bin/jar" --create --file "$lib/fws-lambda-runtime.jar" -C "$build/lambda-java/classes" .
closure "$jdk17" "$rie" > "$build/lambda-java.closure"
mkimage $(accounts lambda-java sbx_user1051 993 990 /home/sbx_user1051) -spec images/lambda/java/image.json -closure "$build/lambda-java.closure" -pkg "$jdk17" -pkg "$rie" \
  -file "/lambda-entrypoint=$build/lambda-entrypoint" -file "/var/runtime/lib=$lib" -file "/var/runtime/bootstrap=images/lambda/java/bootstrap" \
  -out "$out/lambda-java.tar"

# Playwright on Node.js 24, with the playwright packages of the version
# nixpkgs ships and its Chromium headless shell, Chrome for Testing's build,
# checked against the digest pinned here.
pw_version=1.59.1
pw_revision=1217
pw_chrome=147.0.7727.15
case "$(uname -m)" in
  arm64) pw_platform=mac-arm64 pw_sum=92e16fab914827a637607e46468b4360ebd2d30bbad06044d333bb5c0ceb3319 ;;
  x86_64) pw_platform=mac-x64 pw_sum=ef808c5be0d15913f58d44689e2bd114311c8c6b41718fab695cdc1bbcb0e2aa ;;
esac
mkdir -p "$build/playwright/modules" "$build/playwright/browsers/chromium_headless_shell-$pw_revision"
# The install is retried as curl retries below: a runner's resolver drops a
# lookup now and then, and npm retries no failed lookup itself.
for attempt in 1 2 3 4 5; do
  if "$node24/bin/node" "$node24/lib/node_modules/npm/bin/npm-cli.js" install --prefix "$build/playwright/modules" \
    --no-audit --no-fund --no-package-lock "playwright@$pw_version" "playwright-core@$pw_version"; then
    break
  fi
  [ "$attempt" -lt 5 ] || exit 1
  sleep $((attempt * 2))
done
curl -fsSL --retry 5 -o "$build/playwright/shell.zip" \
  "https://storage.googleapis.com/chrome-for-testing-public/$pw_chrome/$pw_platform/chrome-headless-shell-$pw_platform.zip"
echo "$pw_sum  $build/playwright/shell.zip" | shasum -a 256 -c - >/dev/null
unzip -q "$build/playwright/shell.zip" -d "$build/playwright/browsers/chromium_headless_shell-$pw_revision"
closure "$node24" > "$build/playwright.closure"
mkimage $(accounts playwright pwuser 1000 1000 /home/pwuser) -spec images/playwright/image.json -closure "$build/playwright.closure" -pkg "$node24" \
  -file "/entry=$build/playwright-entry" -file "/usr/lib/node_modules=$build/playwright/modules/node_modules" \
  -file "/ms-playwright=$build/playwright/browsers" -out "$out/playwright.tar"

"$build/fws-mkimage" -spec images/toolbox/image.json -file "/toolbox=$build/toolbox" -out "$out/toolbox.tar"

# Ryuk, Testcontainers' reaper, built from its 0.14.0 release.
ryuk_commit=b3726afd6cc2c36628abcc08e9cabac43f587384
ryuk_src=$(GOFLAGS=-mod=mod go mod download -json "github.com/testcontainers/moby-ryuk@$ryuk_commit" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')
cp -R "$ryuk_src" "$build/ryuk-src"
chmod -R u+w "$build/ryuk-src"
(cd "$build/ryuk-src" && GOFLAGS=-mod=mod go build -o "$build/ryuk" .)
"$build/fws-mkimage" -spec images/ryuk/image.json -file "/bin/ryuk=$build/ryuk" \
  -file "/bin/nc=$build/nc" -out "$out/ryuk.tar"

ls -l "$out"
