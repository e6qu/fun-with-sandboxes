#!/usr/bin/env bash
# Build the darwin image archives of the catalog into $1.
# Needs macOS (install_name_tool, codesign) and Nix.
#
# build-images.sh --server-derivations [NAME...] prints the derivations of
# the database servers Nix builds from source (images/servers.nix), and
# build-images.sh --servers [NAME...] builds them and prints their outputs,
# so CI can keep them between runs.
#
# build-images.sh --inputs prints the files the build reads: this script,
# images/, and the Go sources of every package it compiles, so CI can key
# the archives on their content.
set -euo pipefail

# nixos-26.05, pinned, so a rebuild makes the same images.
nixpkgs=${NIXPKGS:-github:NixOS/nixpkgs/9bb52223f4f6a0cc606fd0852d8784f896c137d5}
here=$(cd "$(dirname "$0")/.." && pwd)
nix=(nix --extra-experimental-features 'nix-command flakes')
servers=(--impure -f "$here/images/servers.nix" --argstr nixpkgs "$nixpkgs")
case "${1:-}" in
--inputs)
  cd "$here"
  {
    echo scripts/build-images.sh
    echo go.mod
    echo go.sum
    git ls-files images
    # The module's own packages, file by file; go.sum pins the rest.
    GOOS=darwin GOARCH=arm64 go list -deps -f '{{if .Module}}{{if eq .Module.Path "github.com/e6qu/fun-with-sandboxes"}}{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}}
{{end}}{{range .CFiles}}{{$d}}/{{.}}
{{end}}{{range .HFiles}}{{$d}}/{{.}}
{{end}}{{range .SFiles}}{{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}
{{end}}{{end}}{{end}}' ./cmd/fws-mkimage ./cmd/fws-kube-proxy ./images/... | sed "s|^$here/||"
  } | sort -u
  exit
  ;;
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
go build -o "$build/fws-kube-proxy" ./cmd/fws-kube-proxy
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
go build -o "$build/busybox" ./images/busybox
go build -o "$build/consul-entry" ./images/consul/entry
go build -o "$build/vault-entry" ./images/vault/entry
go build -o "$build/mongo-entry" ./images/mongo/entry
go build -o "$build/ub" ./images/kafka/ub
go build -o "$build/neo4j-entry" ./images/neo4j/entry
go build -o "$build/influxdb-entry" ./images/influxdb/entry
go build -o "$build/dind-entry" ./images/dind/entry
go build -o "$build/dockerd" ./images/dind/dockerd

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
# Mozilla's CA bundle, for the images that carry one.
cacert=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#cacert")
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

# etcd as its release image lays it out: the binaries in /usr/local/bin.
etcd=$(pkg_with etcd etcd)
closure "$etcd" > "$build/etcd.closure"
mkimage $(accounts etcd) -spec images/etcd/image.json -closure "$build/etcd.closure" -pkg "$etcd" \
  -link /usr/local/bin/etcd=../../../bin/etcd -link /usr/local/bin/etcdctl=../../../bin/etcdctl -link /usr/local/bin/etcdutl=../../../bin/etcdutl \
  -out "$out/etcd.tar"

# Consul and Vault, as HashiCorp's images run them: the release binary,
# Vault's as HashiCorp builds it, and an entrypoint at their scripts' path.
consul=$(pkg_with consul consul)
closure "$consul" > "$build/consul.closure"
mkimage $(accounts consul consul 100 1000 /home/consul) -spec images/consul/image.json -closure "$build/consul.closure" -pkg "$consul" \
  -file "/usr/local/bin/docker-entrypoint.sh=$build/consul-entry" -out "$out/consul.tar"

vault=$(pkg_with vault-bin vault)
closure "$vault" > "$build/vault.closure"
mkimage $(accounts vault vault 100 1000 /home/vault) -spec images/vault/image.json -closure "$build/vault.closure" -pkg "$vault" \
  -file "/usr/local/bin/docker-entrypoint.sh=$build/vault-entry" -out "$out/vault.tar"

# MongoDB's own build of the server, mongosh, and the tools the official
# image carries that scripts run in it, Testcontainers' among them: bash,
# openssl and the core utilities.
mongodb=$(pkg_with mongodb-ce mongod)
mongosh=$(pkg_with mongosh mongosh)
openssl=$(pkg_with openssl.bin openssl)
findutils=$(pkg_with findutils find)
coreutils=$(pkg_with coreutils dirname)
gnugrep=$(pkg_with gnugrep grep)
bash=$(pkg_with bash bash)
closure "$mongodb" "$mongosh" "$openssl" "$findutils" "$coreutils" "$gnugrep" "$bash" > "$build/mongo.closure"
mkimage $(accounts mongo mongodb 999 999 /data/db) -spec images/mongo/image.json -closure "$build/mongo.closure" -pkg "$mongodb" -pkg "$mongosh" \
  -pkg "$openssl" -pkg "$findutils" -pkg "$coreutils" -pkg "$gnugrep" -pkg "$bash" \
  -file "/usr/local/bin/docker-entrypoint.sh=$build/mongo-entry" -out "$out/mongo.tar"

# Apache Kafka laid out as Confluent's images lay it out, which
# Testcontainers' module drives: the scripts in /etc/confluent/docker, the
# utility belt they run, and Kafka's tools in /usr/bin without their .sh.
kafka=$(pkg_with apacheKafka kafka-server-start.sh)
coreutils=$(pkg_with coreutils dirname)
gnused=$(pkg_with gnused sed)
bash=$(pkg_with bash bash)
kafka_links=()
for f in "$kafka"/bin/kafka-*.sh; do
  n=$(basename "$f" .sh)
  kafka_links+=(-link "/usr/bin/$n=../../bin/$n.sh")
done
closure "$kafka" "$coreutils" "$gnused" "$bash" > "$build/kafka.closure"
mkimage $(accounts kafka appuser 1000 1000 /home/appuser) -spec images/kafka/image.json -closure "$build/kafka.closure" -pkg "$kafka" \
  -pkg "$coreutils" -pkg "$gnused" -pkg "$bash" "${kafka_links[@]}" \
  -file "/usr/bin/ub=$build/ub" -file "/etc/confluent/docker=images/kafka/docker" -out "$out/kafka.tar"

# Mailpit as its image runs it, from /mailpit.
mailpit=$(pkg_with mailpit mailpit)
closure "$mailpit" > "$build/mailpit.closure"
mkimage $(accounts mailpit) -spec images/mailpit/image.json -closure "$build/mailpit.closure" -pkg "$mailpit" \
  -link /mailpit=bin/mailpit -out "$out/mailpit.tar"

# Meilisearch as its image runs it: /bin/meilisearch, linked from
# /meilisearch, in /meili_data.
meilisearch=$(pkg_with meilisearch meilisearch)
closure "$meilisearch" > "$build/meilisearch.closure"
mkimage $(accounts meilisearch) -spec images/meilisearch/image.json -closure "$build/meilisearch.closure" -pkg "$meilisearch" \
  -link /meilisearch=bin/meilisearch -out "$out/meilisearch.tar"

# Qdrant laid out in /qdrant as its image lays it out: the binary, the
# configuration and entrypoint script from its source, and the web UI
# with the API's OpenAPI document, which its sync script adds.
qdrant=$(pkg_with qdrant qdrant)
qdrant_src=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#qdrant.src")
qdrant_ui=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#qdrant-web-ui")
cp -R "$qdrant_ui" "$build/qdrant-static"
chmod -R u+w "$build/qdrant-static"
cp "$qdrant_src/docs/redoc/master/openapi.json" "$build/qdrant-static/openapi.json"
coreutils=$(pkg_with coreutils dirname)
bash=$(pkg_with bash bash)
closure "$qdrant" "$coreutils" "$bash" > "$build/qdrant.closure"
mkimage $(accounts qdrant) -spec images/qdrant/image.json -closure "$build/qdrant.closure" -pkg "$qdrant" -pkg "$coreutils" -pkg "$bash" \
  -link /usr/bin/env=../../bin/env -link /qdrant/qdrant=../bin/qdrant -file "/qdrant/config=$qdrant_src/config" \
  -file "/qdrant/entrypoint.sh=$qdrant_src/tools/entrypoint.sh" -file "/qdrant/static=$build/qdrant-static" -out "$out/qdrant.tar"

# Neo4j laid out as its image lays it out: NEO4J_HOME at /var/lib/neo4j
# holds the distribution, a writable copy of its configuration, and data
# and logs linked to the /data and /logs volumes; the entrypoint and the
# plugins it installs are in /startup.
neo4j=$(pkg_with neo4j neo4j)
coreutils=$(pkg_with coreutils dirname)
bash=$(pkg_with bash bash)
neo4j_home=$neo4j/share/neo4j
neo4j_args=()
for f in "$neo4j_home"/*; do
  n=$(basename "$f")
  case $n in
  bin | conf | data | logs | run | import | plugins | certificates) ;;
  *) neo4j_args+=(-link "/var/lib/neo4j/$n=../../..$f") ;;
  esac
done
for n in neo4j neo4j-admin cypher-shell; do
  neo4j_args+=(-link "/var/lib/neo4j/bin/$n=../../../../bin/$n")
done
mkdir "$build/neo4j-conf"
cp "$neo4j_home"/conf/* "$build/neo4j-conf"
chmod 777 "$build"/neo4j-conf/*
closure "$neo4j" "$coreutils" "$bash" > "$build/neo4j.closure"
mkimage $(accounts neo4j neo4j 7474 7474 /var/lib/neo4j) -spec images/neo4j/image.json -closure "$build/neo4j.closure" -pkg "$neo4j" \
  -pkg "$coreutils" -pkg "$bash" -link /usr/bin/env=../../bin/env "${neo4j_args[@]}" \
  -link /var/lib/neo4j/data=../../../data -link /var/lib/neo4j/logs=../../../logs -file "/var/lib/neo4j/conf=$build/neo4j-conf" \
  -file "/startup/docker-entrypoint.sh=$build/neo4j-entry" -file "/startup/neo4j-plugins.json=images/neo4j/neo4j-plugins.json" \
  -out "$out/neo4j.tar"

# ClickHouse as its server image runs it: the server configuration its
# packages install, the image's own configuration and entrypoint script,
# which reads the configuration with clickhouse extract-from-config and
# drops to the clickhouse user with clickhouse su.
clickhouse=$(pkg_with clickhouse clickhouse)
clickhouse_docker=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/clickhouse/docker.nix --argstr nixpkgs "$nixpkgs")
mkdir "$build/clickhouse"
cp "$clickhouse_docker"/* "$build/clickhouse"
chmod 755 "$build/clickhouse/entrypoint.sh"
chmod 666 "$build"/clickhouse/*.xml
coreutils=$(pkg_with coreutils dirname)
bash=$(pkg_with bash bash)
gnugrep=$(pkg_with gnugrep grep)
gzip=$(pkg_with gzip gunzip)
wget=$(pkg_with wget wget)
# The time zones Debian's tzdata installs, which ClickHouse resolves TZ in.
tzdata=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#tzdata.out")
closure "$clickhouse" "$coreutils" "$bash" "$gnugrep" "$gzip" "$wget" > "$build/clickhouse.closure"
mkimage $(accounts clickhouse clickhouse 101 101 /var/lib/clickhouse) -spec images/clickhouse/image.json -closure "$build/clickhouse.closure" \
  -pkg "$clickhouse" -pkg "$coreutils" -pkg "$bash" -pkg "$gnugrep" -pkg "$gzip" -pkg "$wget" -link /usr/bin/env=../../bin/env \
  -file "/etc/clickhouse-server/config.xml=$build/clickhouse/config.xml" -file "/etc/clickhouse-server/users.xml=$build/clickhouse/users.xml" \
  -file "/etc/clickhouse-server/config.d/docker_related_config.xml=$build/clickhouse/docker_related_config.xml" \
  -file "/usr/share/zoneinfo=$tzdata/share/zoneinfo" -file "/entrypoint.sh=$build/clickhouse/entrypoint.sh" -out "$out/clickhouse.tar"

# InfluxDB 2 with the influx CLI its image carries, the image's default
# configuration and an entrypoint that does what its script does;
# run-parts runs the user's setup scripts.
influxd=$(pkg_with influxdb2-server influxd)
influx=$(pkg_with influxdb2-cli influx)
debianutils=$(pkg_with debianutils run-parts)
coreutils=$(pkg_with coreutils dirname)
bash=$(pkg_with bash bash)
closure "$influxd" "$influx" "$debianutils" "$coreutils" "$bash" > "$build/influxdb.closure"
mkimage $(accounts influxdb influxdb 1000 1000 /home/influxdb) -spec images/influxdb/image.json -closure "$build/influxdb.closure" \
  -pkg "$influxd" -pkg "$influx" -pkg "$debianutils" -pkg "$coreutils" -pkg "$bash" -link /usr/bin/env=../../bin/env \
  -file "/etc/defaults/influxdb2/config.yml=images/influxdb/default-config.yml" -file "/entrypoint.sh=$build/influxdb-entry" \
  -out "$out/influxdb.tar"

# OpenSearch laid out at /usr/share/opensearch as its image lays it out:
# OpenSearch's minimal distribution, of the version nixpkgs packages, with
# the image's entrypoint and configuration and the changes its one-time
# setup makes at build time, and nixpkgs' JDK at the bundled JDK's path.
opensearch_docker=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/opensearch/docker.nix --argstr nixpkgs "$nixpkgs")
jre=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#jre_headless")
coreutils=$(pkg_with coreutils dirname)
bash=$(pkg_with bash bash)
gnugrep=$(pkg_with gnugrep grep)
gnused=$(pkg_with gnused sed)
# The one-time setup writes paths in the home it runs from, as realpath
# resolves it, which the image then names /usr/share/opensearch.
home=$(cd "$build" && pwd -P)/opensearch
mkdir "$home"
tar -xzf "$opensearch_docker/opensearch-min.tar.gz" -C "$home" --strip-components=1 --exclude '*/jdk/*' --exclude '*/jdk'
cp "$opensearch_docker/opensearch-docker-entrypoint-3.x.sh" "$home/opensearch-docker-entrypoint.sh"
cp "$opensearch_docker/opensearch-onetime-setup.sh" "$home/"
cp "$opensearch_docker/log4j2.properties" "$opensearch_docker/opensearch.yml" "$home/config/"
if [ -d "$home/config/opensearch-performance-analyzer" ]; then
  cp "$opensearch_docker/performance-analyzer.properties" "$home/config/opensearch-performance-analyzer/"
fi
chmod -R u+w "$home"
chmod 755 "$home/opensearch-docker-entrypoint.sh" "$home/opensearch-onetime-setup.sh"
PATH="$gnused/bin:$coreutils/bin:$gnugrep/bin:$PATH" DISABLE_INSTALL_DEMO_CONFIG=true DISABLE_SECURITY_PLUGIN=false \
  "$bash/bin/bash" "$home/opensearch-onetime-setup.sh"
"$gnused/bin/sed" -i "s|$home|/usr/share/opensearch|g" "$home/config/jvm.options"
mkdir -p "$home/data" "$home/logs"
closure "$jre" "$coreutils" "$bash" "$gnugrep" > "$build/opensearch.closure"
mkimage $(accounts opensearch opensearch 1000 1000 /usr/share/opensearch) -spec images/opensearch/image.json -closure "$build/opensearch.closure" \
  -pkg "$coreutils" -pkg "$bash" -pkg "$gnugrep" -link /usr/bin/env=../../bin/env -link "/usr/share/opensearch/jdk=../../..$jre" \
  -file "/usr/share/opensearch=$home" -out "$out/opensearch.tar"

# Docker in Docker as the dind image lays it out: the Docker CLI with its
# buildx and Compose plugins, the entrypoint scripts' work, and a dockerd
# that serves the engine fwsd nests in a privileged container, whose
# socket serves Podman's API too, for the Podman client beside Docker's.
docker=$(pkg_with docker-client docker)
podman=$(pkg_with podman podman)
buildx=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#docker-buildx")
compose=$("${nix[@]}" build --no-link --print-out-paths "$nixpkgs#docker-compose")
coreutils=$(pkg_with coreutils dirname)
wget=$(pkg_with wget wget)
closure "$docker" "$podman" "$buildx" "$compose" "$coreutils" "$wget" > "$build/dind.closure"
mkimage $(accounts dind) -spec images/dind/image.json -closure "$build/dind.closure" -pkg "$docker" -pkg "$podman" -pkg "$coreutils" -pkg "$wget" \
  -link /usr/local/bin/docker=../../../bin/docker -link /usr/bin/env=../../bin/env \
  -link "/usr/local/libexec/docker/cli-plugins/docker-buildx=../../../../..$buildx/libexec/docker/cli-plugins/docker-buildx" \
  -link "/usr/local/libexec/docker/cli-plugins/docker-compose=../../../../..$compose/libexec/docker/cli-plugins/docker-compose" \
  -file "/usr/local/bin/dockerd-entrypoint.sh=$build/dind-entry" -file "/usr/local/bin/docker-entrypoint.sh=$build/dind-entry" \
  -file "/usr/local/bin/dockerd=$build/dockerd" -out "$out/dind.tar"

# Toxiproxy as its image runs it: the server at /toxiproxy and the CLI
# beside it.
toxiproxy=$(pkg_with toxiproxy toxiproxy-server)
closure "$toxiproxy" > "$build/toxiproxy.closure"
mkimage $(accounts toxiproxy) -spec images/toxiproxy/image.json -closure "$build/toxiproxy.closure" -pkg "$toxiproxy" \
  -link /toxiproxy=bin/toxiproxy-server -link /toxiproxy-cli=bin/toxiproxy-cli -out "$out/toxiproxy.tar"

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

# BusyBox as scripts use its image: a POSIX shell and the commands they run
# in it, as separate programs from the GNU tools and others nixpkgs builds
# for darwin, since BusyBox builds for Linux alone, and a busybox command
# that runs them by name. /usr/bin/env is where scripts' shebangs look.
busybox_pkgs=()
for p in coreutils:ls findutils:find diffutils:diff gnugrep:grep gnused:sed gawk:awk gnutar:tar gzip:gzip \
  bzip2:bzip2 xz:xz unzip:unzip wget:wget which:which; do
  busybox_pkgs+=("$(pkg_with "${p%%:*}" "${p#*:}")")
done
closure "${busybox_pkgs[@]}" > "$build/busybox.closure"
mkimage $(accounts busybox) -spec images/busybox/image.json -closure "$build/busybox.closure" \
  $(printf -- '-pkg %s ' "${busybox_pkgs[@]}") -file "/bin/busybox=$build/busybox" \
  -link /usr/bin/env=../../bin/env -out "$out/busybox.tar"

# A general base for images built FROM it, in place of Alpine or Debian,
# whose Linux userlands and package managers cannot run here: bash, the
# commands of fws/busybox, and curl, git, jq and less, with Mozilla's CA
# bundle where Debian's ca-certificates puts it.
base_pkgs=("${busybox_pkgs[@]}")
for p in bash:bash curl:curl git:git jq:jq less:less; do
  base_pkgs+=("$(pkg_with "${p%%:*}" "${p#*:}")")
done
closure "${base_pkgs[@]}" > "$build/base.closure"
mkimage $(accounts base) -spec images/base/image.json -closure "$build/base.closure" \
  $(printf -- '-pkg %s ' "${base_pkgs[@]}") -link /usr/bin/env=../../bin/env \
  -file "/etc/ssl/certs/ca-certificates.crt=$cacert/etc/ssl/certs/ca-bundle.crt" -out "$out/base.tar"

# Ryuk, Testcontainers' reaper, built from its 0.14.0 release.
ryuk_commit=b3726afd6cc2c36628abcc08e9cabac43f587384
ryuk_src=$(GOFLAGS=-mod=mod go mod download -json "github.com/testcontainers/moby-ryuk@$ryuk_commit" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')
cp -R "$ryuk_src" "$build/ryuk-src"
chmod -R u+w "$build/ryuk-src"
(cd "$build/ryuk-src" && GOFLAGS=-mod=mod go build -o "$build/ryuk" .)
"$build/fws-mkimage" -spec images/ryuk/image.json -file "/bin/ryuk=$build/ryuk" \
  -file "/bin/nc=$build/nc" -out "$out/ryuk.tar"

# Kubernetes' control-plane images as its release lays them out: each
# binary in /usr/local/bin over go-runner as the entrypoint, with distroless
# static's accounts and CA bundle. An image holds its own binary and the
# data files the build references, not the other components.
k8s=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/kubernetes/kubernetes.nix --argstr nixpkgs "$nixpkgs")
go_runner=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/kubernetes/go-runner.nix --argstr nixpkgs "$nixpkgs")
closure $(nix-store -qR "$k8s" | grep -v "^$k8s$") > "$build/kubernetes.closure"
for c in kube-apiserver kube-controller-manager kube-scheduler kubectl; do
  mkimage $(accounts "$c" nonroot 65532 65532 /home/nonroot) -spec "images/$c/image.json" -closure "$build/kubernetes.closure" \
    -file "/go-runner=$go_runner/bin/go-runner" -file "/usr/local/bin/$c=$k8s/bin/$c" \
    -file "/etc/ssl/certs/ca-certificates.crt=$cacert/etc/ssl/certs/ca-bundle.crt" -out "$out/$c.tar"
done
pause=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/kubernetes/pause.nix --argstr nixpkgs "$nixpkgs")
closure "$pause" > "$build/pause.closure"
mkimage $(accounts pause) -spec images/pause/image.json -closure "$build/pause.closure" -file "/pause=$pause/bin/pause" -out "$out/pause.tar"
kubelet=$("${nix[@]}" build --no-link --print-out-paths --impure -f images/kubernetes/kubelet.nix --argstr nixpkgs "$nixpkgs")
closure $(nix-store -qR "$kubelet" | grep -v "^$kubelet$") > "$build/kubelet.closure"
mkimage $(accounts kubelet) -spec images/kubelet/image.json -closure "$build/kubelet.closure" \
  -file "/usr/local/bin/kubelet=$kubelet/bin/kubelet" -file "/usr/local/bin/fws-kube-proxy=$build/fws-kube-proxy" \
  -file "/etc/ssl/certs/ca-certificates.crt=$cacert/etc/ssl/certs/ca-bundle.crt" -out "$out/kubelet.tar"

ls -l "$out"
