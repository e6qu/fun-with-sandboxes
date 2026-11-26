# Images

The fws image catalog: what each image holds and how it differs from the
official image it mirrors. How to get the images is in
[USAGE](USAGE.md#get-images); how to write images of your own, and how to
add, change or update a catalog image, is in
[IMAGE-AUTHORING](IMAGE-AUTHORING.md).

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in [RFC 2119](https://www.rfc-editor.org/info/rfc2119).

- [What an fws image is](#what-an-fws-image-is)
- [The catalog](#the-catalog)
- [What every image shares](#what-every-image-shares)
- [Image reference](#image-reference):
  - Base and tools: [base](#fwsbase), [busybox](#fwsbusybox)
  - Languages: [python](#fwspython), [node](#fwsnode), [bun](#fwsbun), [rust](#fwsrust), [lambda](#fwslambdapython-fwslambdanodejs-fwslambdajava), [playwright](#fwsplaywright)
  - Databases: [postgres](#fwspostgres), [mysql](#fwsmysql), [mariadb](#fwsmariadb), [mongo](#fwsmongo), [redis](#fwsredis), [valkey](#fwsvalkey), [memcached](#fwsmemcached), [etcd](#fwsetcd)
  - Search and analytics: [neo4j](#fwsneo4j), [clickhouse](#fwsclickhouse), [influxdb](#fwsinfluxdb), [opensearch](#fwsopensearch), [qdrant](#fwsqdrant), [meilisearch](#fwsmeilisearch)
  - Storage and messaging: [dynamodb-local](#fwsdynamodb-local), [seaweedfs](#fwsseaweedfs), [kafka](#fwskafka), [rabbitmq](#fwsrabbitmq), [nats](#fwsnats)
  - Services: [mailpit](#fwsmailpit), [consul](#fwsconsul), [vault](#fwsvault), [gitea](#fwsgitea), [nginx](#fwsnginx), [toxiproxy](#fwstoxiproxy)
  - Docker and Testcontainers: [dind](#fwsdind), [ryuk](#fwsryuk), [toolbox](#fwstoolbox)
  - Kubernetes: [kube-apiserver and the control plane](#fwskube-apiserver-fwskube-controller-manager-fwskube-scheduler-fwskubectl), [kubelet](#fwskubelet), [kind-node](#fwskind-node), [k3s](#fwsk3s), [pause](#fwspause)

## What an fws image is

An fws image is an ordinary OCI image whose platform is `darwin/arm64`.
Its programs are macOS binaries, run as processes on the Mac; a Linux
image does not run, and a `--platform` other than `darwin/arm64` is
refused. Inside, it is laid out as a Linux image is (`/bin`, `/etc`,
`/usr`, `/var`), and the engine honours its config (`Entrypoint`, `Cmd`,
`Env`, `ExposedPorts`, `Volumes`, `Healthcheck`) as dockerd does.

It differs from a Linux image in two ways:

- **It carries everything it runs**, loaded by relative paths, as a
  container reads nothing of the host but the macOS system tree and the
  shells.
- **It follows the container contract**: its programs find their paths
  and ports at their container values through the container library.

[IMAGE-AUTHORING](IMAGE-AUTHORING.md#the-container-contract) gives the
rules both lead to.

CI publishes each tag `fws/<name>:<tag>` as
`ghcr.io/e6qu/fws-<name>:<tag>`; `fws catalog` lists them
([USAGE](USAGE.md#the-fws-command)).

## The catalog

Each image is configured as the official image is, through the same
variables, files and arguments:

| Image | Configured like the official image through |
|---|---|
| `fws/base:1` | a base to build `FROM` in place of Alpine or Debian: `bash` by default, `fws/busybox`'s commands, `curl`, `git`, `jq`, `less`, and a CA bundle at `/etc/ssl/certs/ca-certificates.crt`; no package manager, as theirs install Linux binaries |
| `fws/busybox:1` | a shell, `sh` by default, and the commands scripts run in BusyBox's image (`ls`, `cat`, `grep`, `sed`, `awk`, `find`, `tar`, `gzip`, `wget`, `nc -z`…) as separate programs, since BusyBox builds for Linux alone; `busybox CMD` runs them by name |
| `fws/python:3.11`, `fws/python:3.12`, `fws/python:3.13`, `fws/python:3.14`, `fws/python:3.15` | the language and its package manager (`python3 -m ensurepip` for pip) |
| `fws/node:22`, `fws/node:24`, `fws/node:26` | `node`, `npm` and `npx`, and on 22 and 24 `corepack` and Yarn 1, which Node.js 25 and its images dropped, in `/usr/local/bin`; `npm install -g` into `/usr/local`; node-gyp builds native addons with the image's `python3`, `make`, `cc`/`c++` (clang with the macOS SDK) and Node.js's headers in `/usr/local/include/node`; `NODE_USE_ENV_PROXY=1`, so Node.js's own `fetch`, corepack's included, goes through the egress proxy; `fws/base`'s commands |
| `fws/bun:1` | `bun` and `bunx` in `/usr/local/bin`, and `bun` as `node` in `/usr/local/bun-node-fallback-bin`; packages' scripts run under `/bin/sh` with `fws/busybox`'s commands |
| `fws/rust:1.100` | Rust 1.100.0, the release's own build: `rustc`, `cargo`, `rustfmt` and `clippy` in `CARGO_HOME` (`/usr/local/cargo`)'s `bin`, first on `PATH`, for `aarch64-apple-darwin`; `cc` (clang with the macOS SDK, which `SDKROOT` names), with which rustc links and the cc crate compiles; `make` and `fws/base`'s commands; no rustup |
| `fws/lambda/python:3.11`, `fws/lambda/python:3.12`, `fws/lambda/python:3.13`, `fws/lambda/python:3.14`, `fws/lambda/nodejs:22`, `fws/lambda/nodejs:24`, `fws/lambda/java:17` | the handler as the command; functions in `/var/task`, invoked at `POST /2015-03-31/functions/function/invocations` on port 8080 through AWS's Runtime Interface Emulator |
| `fws/playwright:1.65` | Node.js 24 with `playwright` and `@playwright/test` 1.65.1 in `/usr/local/lib/node_modules` (`npm install -g` goes to `/usr/local`), so `npx playwright test` runs a mounted project's specs, and the browsers in `/ms-playwright` (`PLAYWRIGHT_BROWSERS_PATH`) that a project's own `@playwright/test@1.65.1` also finds: Chromium's headless shell, which headless launches use, Firefox, which MUST run with `--security-opt seatbelt=gui`, and ffmpeg for videos; no WebKit or headed Chromium ([below](#fwsplaywright)) |
| `fws/postgres:16`, `fws/postgres:18` | `POSTGRES_PASSWORD`, `POSTGRES_USER`, `POSTGRES_DB`, `POSTGRES_HOST_AUTH_METHOD`, `POSTGRES_INITDB_ARGS`, `/docker-entrypoint-initdb.d/*.sql`, `*.sql.gz` and `*.sh`, run against a server that listens on its socket alone; sockets in `/var/run/postgresql`; arguments to `postgres`, or another command, which runs as given |
| `fws/mysql:8.4` | `MYSQL_ROOT_PASSWORD` (or `MYSQL_ALLOW_EMPTY_PASSWORD`/`MYSQL_RANDOM_ROOT_PASSWORD`), `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_HOST`, `MYSQL_ONETIME_PASSWORD`, `/etc/mysql/conf.d/*.cnf`, `/docker-entrypoint-initdb.d/*.sql` and `*.sql.gz`; sockets in `/var/run/mysqld` |
| `fws/mariadb:11` | `MARIADB_ROOT_PASSWORD` (or the `ALLOW_EMPTY`/`RANDOM` forms), `MARIADB_DATABASE`, `MARIADB_USER`, `MARIADB_PASSWORD`, `MARIADB_ROOT_HOST`, their `MYSQL_` names, `/etc/mysql/conf.d/*.cnf`, `/docker-entrypoint-initdb.d/*.sql`; a socket in `/run/mysqld`; a command other than the server runs as given |
| `fws/mongo:8.3` | `MONGO_INITDB_ROOT_USERNAME`, `MONGO_INITDB_ROOT_PASSWORD` (or their `_FILE` forms), `MONGO_INITDB_DATABASE`, `/docker-entrypoint-initdb.d/*.js` and `*.sh`, run against a server that listens on loopback alone; arguments to `mongod`, which binds every address unless they or its configuration name some; `mongosh` |
| `fws/redis:8` | arguments to `redis-server`, with protected mode off unless a configuration file given first sets it, as the official entrypoint does |
| `fws/valkey:9` | arguments to `valkey-server`, or a `.conf` file; protected mode off unless that file sets it |
| `fws/memcached:1.6` | arguments to `memcached` |
| `fws/etcd:3.7` | arguments to `etcd`, with `etcdctl` and `etcdutl`; clients on 2379, peers on 2380 |
| `fws/neo4j:2026.10` | `NEO4J_AUTH`, `NEO4J_PLUGINS` and `NEO4J_` settings, written into `/var/lib/neo4j/conf/neo4j.conf` as the official entrypoint writes them (`NEO4J_server_memory_heap_max__size` becomes `server.memory.heap.max_size`), and mounted `/conf`, `/plugins`, `/import`, `/licenses` and `/ssl`; data in `/data`, logs in `/logs`; HTTP on 7474, Bolt on 7687 |
| `fws/clickhouse:26.10` | `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`, `CLICKHOUSE_DB`, `/docker-entrypoint-initdb.d` and the other variables of the official image's own `entrypoint.sh`, which it runs; configuration in `/etc/clickhouse-server`, data in `/var/lib/clickhouse`; HTTP on 8123, native on 9000 |
| `fws/influxdb:2.10` | `DOCKER_INFLUXDB_INIT_*` setup and upgrade, `*_FILE` secrets and `/docker-entrypoint-initdb.d` scripts, as the official entrypoint handles them; the influx CLI configured in `/etc/influxdb2`, data in `/var/lib/influxdb2`; the API and UI on 8086 |
| `fws/opensearch:3.10` | the minimal distribution under the official image's entrypoint script, so dotted variables such as `discovery.type` become settings; runs as user 1000 from `/usr/share/opensearch`; REST on 9200 |
| `fws/qdrant:1.20` | `QDRANT__` variables over `/qdrant/config`, and `QDRANT_ALLOW_RECOVERY_MODE` as the official entrypoint reads it; storage and snapshots in `/qdrant`; REST and the dashboard on 6333, gRPC on 6334 |
| `fws/meilisearch:1.57` | `MEILI_` variables, such as `MEILI_MASTER_KEY`, and arguments to `meilisearch`, which runs in `/meili_data`; the API on 7700 |
| `fws/dynamodb-local:2` | arguments to DynamoDB Local (default `-sharedDb -dbPath /home/dynamodblocal/data`); port 8000 |
| `fws/seaweedfs:4.54` | the subcommand and its flags, as `mini` (the default: master, volume, filer, S3 on 8333, WebDAV and the admin UI), `server`, `master`, `volume`, `filer`, `s3` or `shell`; `/data` |
| `fws/kafka:4.4` | `KAFKA_` variables, written into `/etc/kafka/kafka.properties` as Confluent's images write them (`KAFKA_ADVERTISED_LISTENERS` becomes `advertised.listeners`), `CLUSTER_ID`, and `KAFKA_LOG4J_ROOT_LOGLEVEL` and `KAFKA_LOG4J_LOGGERS` for its log on standard output, through the `/etc/confluent/docker` scripts Testcontainers' module drives; one KRaft broker and controller by default, clients on 9092; Kafka's tools in `/usr/bin` |
| `fws/rabbitmq:4` | `RABBITMQ_DEFAULT_USER`, `RABBITMQ_DEFAULT_PASS`; arguments to `rabbitmq-server`; AMQP on 5672, the Erlang port mapper on 4369 and distribution on 25672 |
| `fws/nats:2.15` | arguments to `nats-server`; `/nats-server.conf` serves clients on 4222, monitoring on 8222 and routes on 6222 |
| `fws/mailpit:1.34` | `MP_` variables and arguments to `mailpit`; SMTP on 1025, POP3 on 1110, the web UI and API on 8025 |
| `fws/consul:2.1` | `CONSUL_LOCAL_CONFIG`, `CONSUL_BIND_INTERFACE`, `CONSUL_CLIENT_INTERFACE`; arguments to `consul` (default `agent -dev -client 0.0.0.0`); HTTP on 8500, DNS on 8600 |
| `fws/vault:2.2` | `VAULT_DEV_ROOT_TOKEN_ID`, `VAULT_DEV_LISTEN_ADDRESS`, `VAULT_LOCAL_CONFIG`, `VAULT_REDIRECT_INTERFACE`, `VAULT_CLUSTER_INTERFACE`; arguments to `vault` (default `server -dev`); port 8200 |
| `fws/gitea:28` | `GITEA__section__KEY` variables, written into `/data/gitea/conf/app.ini`; HTTP on 3000, SSH on 22 |
| `fws/nginx:1` | `/usr/share/nginx/html`; `/etc/nginx/templates/*.template`, rendered into `conf.d` with the environment variables that are set substituted; `/etc/nginx/nginx.conf`; arguments to `nginx`; the default server listens on 80 |
| `fws/toxiproxy:2.13` | arguments to the server at `/toxiproxy`, with `/toxiproxy-cli` beside it; the API on 8474 |
| `fws/dind:29` | Docker in Docker: run it `--privileged`, and its `dockerd` serves an engine of its own, which the official image's entrypoint arguments, `DOCKER_TLS_CERTDIR` and dockerd's `--host` and TLS flags configure; the Docker CLI with buildx and Compose; 2375, or 2376 with TLS |
| `fws/ryuk:0.14.0` | Testcontainers' reaper ([USAGE](USAGE.md#testcontainers)) |
| `fws/toolbox:1` | the test suite's small app and probe |
| `fws/kube-apiserver:1.37`, `fws/kube-controller-manager:1.37`, `fws/kube-scheduler:1.37`, `fws/kubectl:1.37` | Kubernetes 1.37.2, the release kind's node image ships, laid out as its release images are: the command and its flags, run through `/go-runner` (its `--log-file` and `--also-stdout`), with the binary in `/usr/local/bin`; the API server on 6443, the controller manager on 10257, the scheduler on 10259 |
| `fws/kubelet:1.37` | Kubernetes 1.37.2's kubelet with its darwin parts (`images/kubernetes/kubelet-darwin.patch`): the node's capacity and use from macOS's kernel, and no cgroups; run privileged, it runs its pods on the engine nested in its container, through the CRI that engine serves at `$FWS_ENGINE` |
| `fws/kind-node:1.37` | kind's node image from the parts above: `kind create cluster --image fws/kind-node:1.37` runs kubeadm, the kubelet and `fws-kube-proxy` in each node, with the control plane's images loaded into the node's engine under the names kubeadm gives them ([USAGE](USAGE.md#kind)) |
| `fws/k3s:1.37` | k3s's image, as Testcontainers' k3s module runs it, from `fws/kind-node`'s parts: `k3s server` and the flags it can honour bring up a single-node cluster with kubeadm on the node's address and write `/etc/rancher/k3s/k3s.yaml`; `k3s kubectl`, `ctr` and `crictl` run against it |
| `fws/pause:3.10.2` | Kubernetes' pause program, the infra container of a CRI pod sandbox |

## What every image shares

Each image's section below lists what its spec
(`images/<name>/image.json`), entry program (`images/<name>/entry`) and
build (`scripts/build-images.sh`) give it. What every image shares is said
once here:

- **Common files.** Every image but `fws/ryuk` and `fws/toolbox` carries
  `/bin/sh` (`dash`, but `bash` in `fws/kind-node` and `fws/k3s`),
  `/bin/nc` (a port check alone: `nc -z HOST PORT`), `/etc/services`,
  `/etc/protocols`, and an `/etc/passwd` and `/etc/group` with `root`
  (uid 0) and `nobody` (65534) besides the users its section names, each
  with `/bin/sh` as its shell.
- **Users.** Each user's home but `/nonexistent` exists in the image,
  owned by that user. An image starts as root unless its section names a
  `User`; an entry program that drops to the official image's user does
  it only when it starts as root, giving that user its data directories,
  so `--user` takes precedence. Users are presented, not enforced: every
  process runs as the engine's user.
- **Ports.** A server listens on its container port; the container
  library puts it on a host port, and `-p` publishes it as in Docker.
- **Health checks.** Where a section names `/entry health`, the check runs
  every second with a 5-second timeout, and probes the server on
  `127.0.0.1` at its container port.
- **Variables a program reads itself**, such as Mailpit's `MP_` or
  Meilisearch's `MEILI_`, work as upstream; a section lists those the
  image's spec sets and its entry program reads.

## Image reference

### fws/base

Mirrors a Debian or Alpine base, to build `FROM`. No entry program.

- **Command:** `bash`.
- **Environment:** `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
  `TZ=UTC`, and `SSL_CERT_FILE`, `CURL_CA_BUNDLE` and `GIT_SSL_CAINFO` at
  the CA bundle.
- **Contents:** `fws/busybox`'s commands, `bash`, `curl`, `git`, `jq`,
  `less`; Mozilla's CA bundle at `/etc/ssl/certs/ca-certificates.crt`,
  linked from `/etc/ssl/cert.pem`; `/usr/bin/env` linked to coreutils';
  directories `/root` and `/tmp`.
- **Differs:** no package manager, as Alpine's and Debian's install Linux
  binaries; programs are added with `COPY` or by downloading macOS builds.

### fws/busybox

Mirrors BusyBox's image. No entry program.

- **Command:** `sh`.
- **Environment:** the same `PATH` as `fws/base`, `TZ=UTC`.
- **Contents:** GNU coreutils, findutils, diffutils, grep, sed and awk,
  libarchive's `bsdtar`, gzip, bzip2, xz, unzip, wget and which, as
  separate programs in `/bin`; `/bin/busybox`, which runs the command its
  first argument names from `/bin` and lists them with `--list` (exit
  status 127 for an unknown one); `/bin/tar`, libarchive's tar, which keeps
  a zip read from standard input in a file first, so what it unpacks keeps
  its modes.
- **Differs:** BusyBox builds for Linux alone, so these are not BusyBox's
  applets and their options differ; `nc` only checks a port; there is no
  `ps`, `top` or `vi`, and no CA bundle.

### fws/python

Mirrors `python:<version>` at 3.11, 3.12, 3.13, 3.14 and 3.15. No entry
program.

- **Command:** `python3`, in `/app`.
- **Environment:** `PATH=/bin`, `TZ=UTC`, `PYTHONDONTWRITEBYTECODE=1`,
  `PYTHONUNBUFFERED=1`, `SSL_CERT_FILE` at the CA bundle.
- **Contents:** nixpkgs' Python of the release, with its
  `EXTERNALLY-MANAGED` marker removed so pip installs into it; the CA
  bundle.
- **Differs:** pip is not installed: `python3 -m ensurepip` installs it.
  There is no compiler on `PATH`, so a wheel built from source fails; no
  Debian userland beyond `/bin/sh`.

### fws/node

Mirrors `node:<version>` at 22, 24 and 26. No entry program.

- **Command:** `node`, in `/app`.
- **Environment:** Debian's full `PATH`, `TZ=UTC`,
  `NPM_CONFIG_PREFIX=/usr/local` (so `npm install -g` goes to `/usr/local`,
  not the host's `/usr/lib`), `npm_config_nodedir=/usr/local` (node-gyp
  builds against the image's headers in `/usr/local/include/node`),
  `NODE_USE_ENV_PROXY=1` (Node.js's own `fetch` goes through the egress
  proxy), `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, and
  `SDKROOT` at the macOS SDK.
- **Users:** `node` (1000:1000, home `/home/node`).
- **Contents:** `node`, `npm` and `npx`, and on 22 and 24 `corepack` and
  Yarn 1, in `/bin` and `/usr/local/bin`; `fws/base`'s commands;
  `python3`; nixpkgs' clang with ld64 and the macOS SDK, cctools'
  `libtool`, `lipo` and `otool`, GNU `make`, and `gcc` and `g++` naming
  clang; the CA bundle.
- **Differs:** node-gyp is nixpkgs' build, which takes a fixed Xcode
  version rather than ask `xcodebuild`; there is no Xcode, `xcrun` or
  Swift.

### fws/bun

Mirrors `oven/bun:1` (1.4.5). No entry program.

- **Command:** `bun`, in `/app`.
- **Environment:** Debian's full `PATH` ending with
  `/usr/local/bun-node-fallback-bin`, `TZ=UTC`,
  `BUN_RUNTIME_TRANSPILER_CACHE_PATH=0`, `BUN_INSTALL_BIN=/usr/local/bin`.
- **Users:** `bun` (1000:1000, home `/home/bun`).
- **Contents:** Bun's own macOS build: `bun` and `bunx` in `/usr/local/bin`,
  and `bun` as `node` in `/usr/local/bun-node-fallback-bin`; `fws/busybox`'s
  commands, which packages' scripts run under `/bin/sh`.

### fws/rust

Mirrors `rust:1.100`. No entry program.

- **Command:** `bash`.
- **Environment:** `PATH` led by `/usr/local/cargo/bin`, `TZ=UTC`,
  `CARGO_HOME=/usr/local/cargo`, `RUST_VERSION=1.100.0`, the three CA
  variables, and `SDKROOT` at the macOS SDK, which rustc's link and the cc
  crate find.
- **Contents:** Rust 1.100.0's own macOS build (`rustc`, `cargo`, the
  standard library, `rustfmt`, `clippy`) in `CARGO_HOME`'s `bin`; the C
  toolchain `fws/node` has; `fws/base`'s commands; the CA bundle.
- **Differs:** no rustup and no `RUSTUP_HOME`: the toolchain is for
  `aarch64-apple-darwin` alone, `rustup` commands and other targets are
  absent, and a `rust-toolchain.toml` goes unread.

### fws/lambda/python, fws/lambda/nodejs, fws/lambda/java

Mirror AWS's Lambda base images: `fws/lambda/python` at 3.11 to 3.14,
`fws/lambda/nodejs` at 22 and 24, `fws/lambda/java` at 17.

- **Entrypoint:** `/lambda-entrypoint`, which takes the handler as its one
  argument (any other count exits with status 142, as AWS's does), sets
  `_HANDLER`, and runs the runtime, `/var/runtime/bootstrap`, under AWS's
  Runtime Interface Emulator (`/bin/aws-lambda-rie`), or alone where
  `AWS_LAMBDA_RUNTIME_API` names a Runtime API already. The default
  handlers are `app.handler`, `index.handler` and
  `example.App::handleRequest`.
- **Environment:** `PATH=/bin`, `TZ=UTC`, `LANG=en_US.UTF-8`,
  `LAMBDA_TASK_ROOT=/var/task` (the working directory),
  `LAMBDA_RUNTIME_DIR=/var/runtime`, `AWS_EXECUTION_ENV`
  (`AWS_Lambda_python3.14`, `AWS_Lambda_nodejs24.x`,
  `AWS_Lambda_java17`), `SSL_CERT_FILE`, and on Node.js
  `NODE_USE_ENV_PROXY=1`.
- **Ports:** 8080, where the emulator takes invocations at
  `POST /2015-03-31/functions/function/invocations`, and 9001, the Runtime
  API. Health: a TCP connection to 8080, every second.
- **Users:** `sbx_user1051` (993:990, home `/home/sbx_user1051`), which no
  `User` selects.
- **Differs:** the runtime is the image's own client of the Runtime API,
  not AWS's runtime interface client: `bootstrap.py` loads
  `module.function` from `LAMBDA_TASK_ROOT`; `bootstrap.mjs` loads
  `file.function` from an `.mjs`, `.js` or `.cjs` module there, returning a
  promise or taking a callback; Java's `fws.lambda.Bootstrap` takes
  `package.Class::method`, or a class that is a `RequestHandler` or
  `RequestStreamHandler`, loading classes from `LAMBDA_TASK_ROOT` and its
  `lib` jars, with `aws-lambda-java-core` 1.2.3 and Jackson 2.17.2 in
  `/var/runtime/lib`, and runs the JVM with `-XX:TieredStopAtLevel=1`.
  The runtimes set `AWS_EXECUTION_ENV` themselves, since the emulator keeps
  it from them. Python's `EXTERNALLY-MANAGED` marker is removed.

### fws/playwright

Mirrors Microsoft's Playwright image at 1.65.

- **Entrypoint:** `/entry`, which runs its arguments (by default `node`) as
  given, found on the container's `PATH`; the image's configuration, not
  the entry program, sets Playwright up, so `docker exec` finds the same.
- **Environment:** `PATH=/bin`, `TZ=UTC`, `NPM_CONFIG_PREFIX=/usr/local`,
  `NODE_PATH=/usr/local/lib/node_modules`,
  `PLAYWRIGHT_BROWSERS_PATH=/ms-playwright`,
  `MOZ_DISABLE_CONTENT_SANDBOX`, `MOZ_DISABLE_GPU_SANDBOX`,
  `MOZ_DISABLE_RDD_SANDBOX`, `MOZ_DISABLE_SOCKET_PROCESS_SANDBOX`,
  `MOZ_DISABLE_UTILITY_SANDBOX` and `MOZ_DISABLE_GMP_SANDBOX` (all `1`),
  `SSL_CERT_FILE`, `NODE_USE_ENV_PROXY=1`. Working directory `/app`.
- **Users:** `pwuser` (1000:1000, home `/home/pwuser`), which no `User`
  selects.
- **Contents:** Node.js 24; `playwright`, `playwright-core` and
  `@playwright/test` 1.65.1 in `/usr/local/lib/node_modules`, with
  `playwright` in `/bin` and `/usr/local/bin`, so `npx playwright test`
  runs the image's runner and fetches none; under `/ms-playwright`,
  Chromium's headless shell (Chrome for Testing 159.0.8154.6, revision
  1255), Firefox (revision 1572) and ffmpeg (revision 1013), which a
  project's own `@playwright/test@1.65.1` also finds; coreutils.
- **Requires:** Firefox MUST run with `--security-opt seatbelt=gui`, as
  the engine's user (`--user "$(id -u):$(id -g)"`; the end-to-end tests
  also set `-e HOME=/tmp`): macOS Firefox starts AppKit even headless,
  which registers the process with the window server and Launch Services,
  and the container's default profile denies those. A launch that passes its own
  environment MUST keep the `MOZ_DISABLE_*_SANDBOX` variables. Chromium
  MUST run without its own sandbox, as Playwright launches it by default:
  a launch MUST NOT set `chromiumSandbox: true`.
- **Differs:** no full Chromium, which aborts as it starts under the
  container's profile, so headed Chromium and `channel: 'chromium'` are
  absent. No WebKit: Playwright's WebKit on macOS starts its web content,
  network and GPU processes as XPC services, which launchd runs outside the
  container's sandbox and library, and its builds differ by macOS release,
  which an image cannot choose for its host.

### fws/postgres

Mirrors `postgres:16` and `postgres:18`.

- **Entrypoint:** `/entry`; command `postgres`; stop signal `SIGINT`.
  Arguments that are `postgres` or start with `-` go to the server after
  the image's own; any other command runs as given, with no database set
  up. Health: `pg_isready -h 127.0.0.1 -U $POSTGRES_USER -d postgres`.
- **Environment read:** `POSTGRES_PASSWORD`; `POSTGRES_HOST_AUTH_METHOD`
  (`scram-sha-256` with a password by default; `trust` without one);
  `POSTGRES_USER` (default `postgres`); `POSTGRES_DB` (default the user);
  `POSTGRES_INITDB_ARGS`; `PGDATA` (default `/var/lib/postgresql/data`).
  Without a password or `trust`, an uninitialised container exits with the
  official image's message.
- **Ports:** 5432. Volume `/var/lib/postgresql/data`.
- **Users:** `postgres` (999:999, home `/var/lib/postgresql`), which the
  server runs as.
- **Paths:** sockets in `/var/run/postgresql`, as the official image's,
  and in `/tmp`, where the image's libpq looks, so `psql` in the container
  needs no host.
- **Initialisation:** `initdb -E UTF8 --no-locale`, local connections
  trusted; then `/docker-entrypoint-initdb.d/*.sql`, `*.sql.gz` and `*.sh`
  (an executable one run directly, else by `/bin/sh`), in name order,
  through a server on its socket alone; other files are ignored.
- **Differs:** macOS allows 87 System V semaphores machine-wide, so the
  server runs with `max_connections=10`, `max_worker_processes=2`,
  `max_parallel_workers=0`, `autovacuum_max_workers=1`,
  `max_wal_senders=0`, `wal_level=minimal` and
  `dynamic_shared_memory_type=mmap`; `-c` arguments given to the container
  come after and override them. `*_FILE` variables and
  `POSTGRES_INITDB_WALDIR` are not read.

### fws/mysql

Mirrors `mysql:8.4` (8.4.13, built with the vendor's compilation comment).

- **Entrypoint:** `/entry`; command `mysqld`. Arguments that are `mysqld`
  or start with `-` go to the server; any other command runs as given.
  Health: the server's greeting on 3306.
- **Environment read:** `MYSQL_ROOT_PASSWORD`, or
  `MYSQL_ALLOW_EMPTY_PASSWORD`, or `MYSQL_RANDOM_ROOT_PASSWORD` (the
  password is logged as `GENERATED ROOT PASSWORD`), one of which an
  uninitialised container requires; `MYSQL_ROOT_HOST` (default `%`);
  `MYSQL_ONETIME_PASSWORD`; `MYSQL_DATABASE`; `MYSQL_USER` (not `root`) and
  `MYSQL_PASSWORD`.
- **Ports:** 3306 and 33060. Volume `/var/lib/mysql`.
- **Users:** `mysql` (999:999, home `/var/lib/mysql`).
- **Paths:** the official image's `/etc/my.cnf`, which includes
  `/etc/mysql/conf.d/*.cnf`; sockets `/var/run/mysqld/mysqld.sock` and
  `mysqlx.sock`; `secure-file-priv` `/var/lib/mysql-files`. The entry
  names the server's base, message, plugin, data and temporary
  directories on its command line, which override the configuration.
- **Initialisation:** `mysqld --initialize-insecure`, then the users and
  database, then `/docker-entrypoint-initdb.d/*.sql` and `*.sql.gz`, each
  sent to the server as one batch after `USE` of `MYSQL_DATABASE`, through
  a server with networking off.
- **Differs:** init scripts go to the server, not through the `mysql`
  client, so client commands such as `DELIMITER` do not work; `*.sh`
  scripts and `*_FILE` variables are not read.

### fws/mariadb

Mirrors `mariadb:11` (11.4.14, built with the vendor's compilation
comment).

- **Entrypoint:** `/entry`; command `mariadbd`. Arguments that are
  `mariadbd`, `mysqld` or start with `-` go to the server; any other
  command runs as given. Health: the server's greeting on 3306.
- **Environment read:** each under its `MARIADB_` name or its `MYSQL_`
  one: `ROOT_PASSWORD`, `ALLOW_EMPTY_ROOT_PASSWORD` (`MYSQL_ALLOW_EMPTY_PASSWORD`),
  `RANDOM_ROOT_PASSWORD`, `ROOT_HOST` (default `%`), `DATABASE`, `USER`,
  `PASSWORD`.
- **Ports:** 3306. Volume `/var/lib/mysql`.
- **Users:** `mysql` (999:999, home `/var/lib/mysql`).
- **Paths:** the official image's `/etc/mysql/my.cnf`, which names the
  socket `/run/mysqld/mysqld.sock` and includes
  `/etc/mysql/mariadb.conf.d/` and `/etc/mysql/conf.d/`, for settings
  mounted or copied in. The entry names the server's base, data, message,
  plugin and temporary directories, socket and pid file on its command
  line, which override the configuration; the socket by its container
  path, which the container library binds.
- **Initialisation:** the system tables from the server's own scripts
  through `mariadbd --bootstrap`, as `mariadb-install-db` would feed them;
  then anonymous users are dropped, the users and database made, and
  `/docker-entrypoint-initdb.d/*.sql` run after `USE` of the database.
- **Differs:** the server runs with `--skip-name-resolve`. Only `*.sql`
  init scripts run (no `*.sql.gz` or `*.sh`), and `*_FILE` variables are
  not read.

### fws/mongo

Mirrors `mongo:8.3` with MongoDB's own build and `mongosh`.

- **Entrypoint:** `/usr/local/bin/docker-entrypoint.sh`; command `mongod`.
  An option as the first argument runs `mongod` with it; `mongod` and
  `mongos` run as `mongodb`; any other command runs as given. No health
  check.
- **Environment read:** `MONGO_INITDB_ROOT_USERNAME` and
  `MONGO_INITDB_ROOT_PASSWORD`, both or neither, or their `_FILE` forms;
  `MONGO_INITDB_DATABASE` (default `test`) for `*.js` scripts. Every
  `MONGO_INITDB_` variable is unset before `mongod` starts. Also set:
  `PATH=/usr/local/bin:/bin`, `HOME=/data/db`.
- **Ports:** 27017. Volumes `/data/db` and `/data/configdb`.
- **Users:** `mongodb` (999:999, home `/data/db`).
- **Initialisation:** with a root user or init scripts and an empty data
  directory, a `mongod` on `127.0.0.1` alone, without authentication,
  creates the root user and runs `/docker-entrypoint-initdb.d/*.sh`
  (sourced by `bash`) and `*.js` (through `mongosh`); then `mongod` starts
  with `--auth` where a root user was made.
- **Differs:** `mongod` binds every address (`--bind_ip_all`) unless its
  arguments or configuration name addresses; the setup server stops on
  `SIGTERM`, as `mongod --shutdown` is Linux's alone. The image carries
  `bash`, `openssl`, coreutils, findutils and grep for scripts.

### fws/redis

Mirrors `redis:8`.

- **Entrypoint:** `/entry`; command `redis-server`, in `/data`. An option
  or a `.conf` file as the first argument runs `redis-server` with it; any
  other command runs as given. Health: `PING` on 6379.
- **Ports:** 6379. Volume `/data`.
- **Users:** `redis` (999:999, home `/data`), which `redis-server` runs
  as, under umask 0077, owning the working directory.
- **Differs:** none in behaviour: protected mode is off unless a
  configuration file given first sets `protected-mode`, as the official
  entrypoint does.

### fws/valkey

Mirrors `valkey/valkey:9` (9.2.0), with `fws/redis`'s entry program for
`valkey-server`.

- **Entrypoint:** `/entry`; command `valkey-server`, in `/data`; as
  `fws/redis`, with protected mode off unless a configuration file given
  first sets it. Health: `PING` on 6379.
- **Ports:** 6379. Volume `/data`.
- **Users:** `valkey` (999:999, home `/data`).

### fws/memcached

Mirrors `memcached:1.6`.

- **Entrypoint:** `/entry`; command `memcached`; `User` `memcache`. An
  option as the first argument runs `memcached` with it; any other command
  runs as given. Health: `version` on 11211.
- **Ports:** 11211.
- **Users:** `memcache` (11211:11211, home `/home/memcache`).

### fws/etcd

Mirrors etcd's release image at 3.7 (3.7.4). No entry program.

- **Command:** `/usr/local/bin/etcd`, in `/var/lib/etcd`.
- **Environment:** `PATH=/usr/local/bin:/bin`, `TZ=UTC`.
- **Ports:** 2379 (clients), 2380 (peers).
- **Contents:** `etcd`, `etcdctl` and `etcdutl` in `/usr/local/bin`;
  directories `/var/etcd` and `/var/lib/etcd`.
- **Differs:** a shell and `nc` where upstream's image is distroless. As
  upstream, etcd listens on `localhost` unless `--listen-client-urls` and
  `--advertise-client-urls` say otherwise, as the end-to-end tests give
  them.

### fws/neo4j

Mirrors `neo4j:2026.10`, Community Edition.

- **Entrypoint:** `/startup/docker-entrypoint.sh`; command `neo4j`, in
  `/var/lib/neo4j`. `neo4j` runs the server that `neo4j console --dry-run`
  prints, as `neo4j`; `dump-config` copies the configuration to a mounted
  `/conf`; any other command runs as given after the configuration is
  written.
- **Environment read:** `NEO4J_AUTH` (`neo4j/<password>` or `none`) and
  `NEO4J_AUTH_PATH`; `NEO4J_PLUGINS` (a JSON list) and its old name
  `NEO4JLABS_PLUGINS`; every other `NEO4J_<setting>`, written into
  `/var/lib/neo4j/conf/neo4j.conf` with `_` as a dot and `__` as an
  underscore (`apoc.*` settings into `apoc.conf`), and its `_FILE` form;
  the 4.x names `NEO4J_dbms_tx__log_rotation_retention__policy`,
  `NEO4J_dbms_memory_pagecache_size` and
  `NEO4J_dbms_default__listen__address`; `EXTENSION_SCRIPT`,
  `EXTENDED_CONF`, `NEO4J_DEBUG`. Also set: `NEO4J_HOME=/var/lib/neo4j`,
  `NEO4J_EDITION=community`, `LANG=C.UTF-8`, and `PATH` led by
  `/var/lib/neo4j/bin`.
- **Ports:** 7473, 7474 (HTTP), 7687 (Bolt). Volumes `/data` and `/logs`.
- **Users:** `neo4j` (7474:7474, home `/var/lib/neo4j`).
- **Paths:** a mounted `/conf` replaces the configuration, `/ssl` becomes
  the certificates directory, and `/plugins`, `/import`, `/logs` and
  `/licenses` become the server's directories; defaults
  `server.memory.pagecache.size=512M` and
  `server.default_listen_address=0.0.0.0` where the configuration sets
  neither. Plugins come from the image's files where
  `/startup/neo4j-plugins.json` locates one, else from the jar its versions
  list names for this Neo4j.
- **Differs:** Enterprise Edition is absent, so its license variable
  applies to nothing here.

### fws/clickhouse

Mirrors `clickhouse/clickhouse-server:26.10` (26.10.8.34-stable), with
ClickHouse's own macOS build.

- **Entrypoint:** `/entrypoint.sh`, the official image's own script at the
  release, with its variables: `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`,
  `CLICKHOUSE_DB` and the rest it reads, and `/docker-entrypoint-initdb.d`.
- **Environment:** `PATH=/usr/bin:/bin`, `LANG=en_US.UTF-8`, `TZ=UTC`,
  `CLICKHOUSE_CONFIG=/etc/clickhouse-server/config.xml`.
- **Ports:** 8123 (HTTP), 9000 (native), 9009. Volume
  `/var/lib/clickhouse`.
- **Users:** `clickhouse` (101:101, home `/var/lib/clickhouse`).
- **Paths:** the server configuration the release's packages install and
  the image's `docker_related_config.xml`, in `/etc/clickhouse-server`;
  logs directory `/var/log/clickhouse-server`; Debian's time zones in
  `/usr/share/zoneinfo`. `bash`, coreutils, grep, gzip and wget serve the
  script.

### fws/influxdb

Mirrors `influxdb:2.10` (2.10.1), with the `influx` CLI.

- **Entrypoint:** `/entrypoint.sh`, a Go program doing what the official
  script does; command `influxd`. `influxd`, `run` or options run the
  server as `influxdb`; any other command runs as given.
- **Environment read:** `DOCKER_INFLUXDB_INIT_MODE` (`setup` or
  `upgrade`), `DOCKER_INFLUXDB_INIT_USERNAME`, `_PASSWORD`, `_ORG`,
  `_BUCKET`, `_RETENTION`, `_ADMIN_TOKEN` (and the `_FILE` forms of the
  username, password and token), `_UPGRADE_V1_CONFIG`, `_UPGRADE_V1_DIR`,
  `_CLI_CONFIG_NAME` (default `default`); `INFLUXD_CONFIG_PATH`; the
  `INFLUXD_` forms of `bolt-path`, `engine-path`, `log-level`, `tls-key`,
  `tls-cert` and `http-bind-address`; `INFLUXD_INIT_PORT` (default 9999)
  and `INFLUXD_INIT_PING_ATTEMPTS` (default 600) for the temporary setup
  server; `INFLUX_CONFIGS_PATH` (default
  `/etc/influxdb2/influx-configs`).
- **Ports:** 8086. Volumes `/etc/influxdb2` and `/var/lib/influxdb2`.
- **Users:** `influxdb` (1000:1000, home `/home/influxdb`).
- **Paths:** the configuration is `INFLUXD_CONFIG_PATH`, else the first of
  `/etc/influxdb2/config.json`, `.toml`, `.yaml` and `.yml`, else
  `/etc/defaults/influxdb2/config.yml` (bolt and engine paths under
  `/var/lib/influxdb2`); executable `*.sh` scripts in
  `/docker-entrypoint-initdb.d` run against the setup server.
- **Differs:** none in behaviour; a failed setup removes the half-made
  database so the next start sets up again, as the official script does.

### fws/opensearch

Mirrors `opensearchproject/opensearch:3.10` (3.10.0).

- **Entrypoint:** `./opensearch-docker-entrypoint.sh`, the official 3.x
  script, in `/usr/share/opensearch`; command `opensearch`; `User` `1000`.
  Its variables apply: dotted names such as `discovery.type` become
  settings, `OPENSEARCH_INITIAL_ADMIN_PASSWORD` sets the demo admin's
  password, and `DISABLE_INSTALL_DEMO_CONFIG` and
  `DISABLE_SECURITY_PLUGIN` turn those parts off.
- **Environment:** `PATH`, `JAVA_HOME=/usr/share/opensearch/jdk`, and the
  official image's `LD_LIBRARY_PATH`, which names the k-NN and
  neural-search plugins' directories this image does not have.
- **Ports:** 9200 (REST), 9300, 9600, 9650.
- **Users:** `opensearch` (1000:1000, home `/usr/share/opensearch`).
- **Differs:** the minimal distribution with the security plugin alone,
  without k-NN and ML Commons, whose native libraries are built for Linux;
  nixpkgs' headless JRE at the bundled JDK's path; the one-time setup ran
  at build time. With security on, as by default, REST serves HTTPS.

### fws/qdrant

Mirrors `qdrant/qdrant` at 1.20 (1.20.2), with its web UI (0.2.21).

- **Command:** `./entrypoint.sh`, Qdrant's own script, in `/qdrant`; no
  entrypoint, so a command replaces it.
- **Environment:** `PATH=/usr/bin:/bin`, `TZ=Etc/UTC`,
  `RUN_MODE=production`; `QDRANT__` variables override `/qdrant/config`,
  and the script reads `QDRANT_ALLOW_RECOVERY_MODE`.
- **Ports:** 6333 (REST and the dashboard), 6334 (gRPC).
- **Paths:** the binary at `/qdrant/qdrant`, the configuration in
  `/qdrant/config`, the web UI with the API's OpenAPI document in
  `/qdrant/static`; storage and snapshots in `/qdrant`.

### fws/meilisearch

Mirrors `getmeili/meilisearch` at 1.57 (1.57.0).

- **Command:** `/bin/sh -c /bin/meilisearch`, in `/meili_data`; no
  entrypoint. `/meilisearch` links to the binary.
- **Environment:** `PATH=/usr/bin:/bin`, `TZ=UTC`,
  `MEILI_HTTP_ADDR=0.0.0.0:7700`, `MEILI_SERVER_PROVIDER=docker`; other
  `MEILI_` variables such as `MEILI_MASTER_KEY` are Meilisearch's own.
- **Ports:** 7700. The spec declares no volume.

### fws/dynamodb-local

Mirrors `amazon/dynamodb-local:2`.

- **Entrypoint:** `/entry`, in `/home/dynamodblocal`: it runs
  `java -jar DynamoDBLocal.jar -port 8000` with its native libraries and
  `/tmp` as Java's temporary directory, and its arguments after them
  (default `-sharedDb -dbPath /home/dynamodblocal/data`). Health: an HTTP
  request to 8000.
- **Environment:** `PATH=/bin`, `TZ=UTC`, `DYNAMODB_HOME` at the package.
- **Ports:** 8000.
- **Users:** `dynamodblocal` (1000:1000, home `/home/dynamodblocal`).
- **Paths:** `DynamoDBLocal.jar` and `DynamoDBLocal_lib` are linked into
  the working directory, so an entrypoint of `java` and a command of
  `-jar DynamoDBLocal.jar ...`, as Testcontainers' module runs them, work.
- **Differs:** the port is always 8000; a `-port` argument is passed after
  it.

### fws/seaweedfs

Mirrors `chrislusf/seaweedfs` at 4.54.

- **Entrypoint:** `/entry`; command `mini -dir=/data`, in `/data`. It sets
  `GODEBUG=fips140=on` (added to any `GODEBUG` given), runs as `seaweed`
  owning `/data`, and gives each subcommand the official script's
  defaults, all with `-logtostderr=true`: `master` with `-mdir=/data
  -volumeSizeLimitMB=1024`; `volume` with `-dir=/data -max=0` (no `-max`
  where given); `server` with `-dir=/data -volume.max=0
  -master.volumeSizeLimitMB=1024`; `mini` with `-dir=/data` unless given;
  `filer`; `s3` with `S3_DOMAIN_NAME`, `S3_KEY_FILE` and `S3_CERT_FILE`;
  `shell` with `SHELL_CLUSTER`, `SHELL_FILER`, `SHELL_FILER_GROUP`,
  `SHELL_MASTER` and `SHELL_OPTIONS`, its arguments as the shell's input.
  Any other subcommand goes to `weed` as given. No health check.
- **Ports:** 7333, 8080, 8333 (S3), 8888, 9333, 18080, 18888, 19333.
  Volume `/data`.
- **Users:** `seaweed` (1000:1000, home `/home/seaweed`).
- **Paths:** `/usr/bin/weed`; `/etc/seaweedfs/filer.toml` keeps the filer
  in LevelDB at `/data/filerldb2`.
- **Requires:** a network with a subnet where SeaweedFS advertises an
  address ([LIMITATIONS](../LIMITATIONS.md#networking)).

### fws/kafka

Mirrors Confluent's Kafka image layout with Apache Kafka 4.4.0 (Scala
2.13).

- **Command:** `/etc/confluent/docker/run`, as `appuser`, in
  `/home/appuser`; no entrypoint. Its `NAME=VALUE` arguments set variables
  first. It runs `configure`, then `ensure`, then `launch`, as
  Confluent's does; `TRACE=true` traces them.
- **Environment:** defaults for one KRaft broker and controller:
  `CLUSTER_ID=4L6g3nShT-eMCtK--X86sw`, `KAFKA_NODE_ID=1`,
  `KAFKA_PROCESS_ROLES=broker,controller`,
  `KAFKA_LISTENERS=PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093`,
  `KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092`,
  `KAFKA_LISTENER_SECURITY_PROTOCOL_MAP`,
  `KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER`,
  `KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093`, and replication
  factors and minimum in-sync replicas of 1. Every `KAFKA_` variable
  becomes a property in `/etc/kafka/kafka.properties`
  (`KAFKA_ADVERTISED_LISTENERS` becomes `advertised.listeners`);
  `KAFKA_LISTENERS`, where unset, is the advertised listeners on
  `0.0.0.0`; `KAFKA_LOG_DIRS` defaults to `/var/lib/kafka/data`.
  `KAFKA_LOG4J_ROOT_LOGLEVEL` (default `INFO`) and `KAFKA_LOG4J_LOGGERS`
  (`name=LEVEL,...`) set the broker's log on standard output,
  `KAFKA_TOOLS_LOG4J_LOGLEVEL` (default `WARN`) the tools' on standard
  error, and `KAFKA_LOG4J_OPTS` replaces either.
- **Ports:** 9092. Volumes `/etc/kafka/secrets` and `/var/lib/kafka/data`.
- **Users:** `appuser` (1000:1000, home `/home/appuser`).
- **Paths:** Kafka's tools in `/usr/bin` without their `.sh`
  (`kafka-topics`), and the `ub` utility the scripts run.
- **Differs:** Apache Kafka, not Confluent Platform: no REST Proxy or other
  Confluent components; `ensure` formats the log directories with
  `kafka-storage format --ignore-formatted`.

### fws/rabbitmq

Mirrors `rabbitmq:4` (4.4.1).

- **Entrypoint:** `/entry`; command `rabbitmq-server`, as `rabbitmq`, with
  the arguments that follow it in the command. Health: an AMQP handshake
  on 5672.
- **Environment:** `RABBITMQ_DEFAULT_USER` and `RABBITMQ_DEFAULT_PASS` are
  RabbitMQ's own. Where unset, the entry sets `RABBITMQ_NODENAME=rabbit@localhost`,
  `RABBITMQ_MNESIA_BASE` and `RABBITMQ_LOG_BASE` under `/var/lib/rabbitmq`,
  `RABBITMQ_LOGS=-` (the log on standard output),
  `RABBITMQ_CONFIG_FILE=/etc/rabbitmq/rabbitmq`,
  `RABBITMQ_ADVANCED_CONFIG_FILE=/etc/rabbitmq/advanced.config` and
  `RABBITMQ_ENABLED_PLUGINS_FILE=/etc/rabbitmq/enabled_plugins`, which it
  writes as `[].` (no plugins) if absent; `HOME` is always
  `/var/lib/rabbitmq`. Also set: `LANG=C.UTF-8`.
- **Ports:** 5672 (AMQP), 4369 (the Erlang port mapper), 25672
  (distribution). Volume `/var/lib/rabbitmq`.
- **Users:** `rabbitmq` (999:999, home `/var/lib/rabbitmq`).
- **Differs:** the image runs `rabbitmq-server` alone: another command
  fails with `unknown command`.

### fws/nats

Mirrors `nats:2.15`. No entry program.

- **Entrypoint:** `/bin/nats-server`; command `--config /nats-server.conf`.
- **Ports:** 4222 (clients), 8222 (monitoring), 6222 (routes).
- **Paths:** `/nats-server.conf`, the official image's: clients on 4222,
  monitoring on 8222, cluster `my_cluster` on 6222 with route
  authorization and no routes.

### fws/mailpit

Mirrors `axllent/mailpit` at 1.34 (1.34.0). No entry program.

- **Entrypoint:** `/mailpit`; its arguments and `MP_` variables configure
  it. Health: `/mailpit readyz` every 15 seconds, every second during a
  10-second start period.
- **Ports:** 1025 (SMTP), 1110 (POP3), 8025 (web UI and API).

### fws/consul

Mirrors `hashicorp/consul:2.1`.

- **Entrypoint:** `/usr/local/bin/docker-entrypoint.sh`; command
  `agent -dev -client 0.0.0.0`. An option, `version` or a command
  `consul --help` lists runs `consul`; `agent` gets
  `-data-dir=/consul/data -config-dir=/consul/config` and the bind and
  client addresses below; `consul` runs as `consul` unless
  `CONSUL_DISABLE_PERM_MGMT` is set; any other command runs as given.
- **Environment read:** `CONSUL_LOCAL_CONFIG`, written to
  `/consul/config/local.json`; `CONSUL_BIND_INTERFACE` and
  `CONSUL_CLIENT_INTERFACE`, whose first IPv4 address becomes `-bind` and
  `-client` (the interfaces are the presented ones: loopback and `eth0`);
  `CONSUL_DISABLE_PERM_MGMT`.
- **Ports:** 8300, 8301 and 8302 (TCP and UDP), 8500 (HTTP), 8600 (DNS,
  TCP and UDP). Volume `/consul/data`.
- **Users:** `consul` (uid 100, gid 1000, home `/home/consul`).

### fws/vault

Mirrors `hashicorp/vault:2.2` (2.2.2), with HashiCorp's macOS build.

- **Entrypoint:** `/usr/local/bin/docker-entrypoint.sh`; command
  `server -dev`. `server` gets `-config=/vault/config`,
  `-dev-root-token-id=$VAULT_DEV_ROOT_TOKEN_ID` and
  `-dev-listen-address` (`$VAULT_DEV_LISTEN_ADDRESS`, default
  `0.0.0.0:8200`); an option, `version` or a command `vault --help` lists
  runs `vault`, as `vault`; any other command runs as given.
- **Environment read:** `VAULT_DEV_ROOT_TOKEN_ID`,
  `VAULT_DEV_LISTEN_ADDRESS`, `VAULT_LOCAL_CONFIG` (written to
  `/vault/config/local.json`), `VAULT_REDIRECT_INTERFACE` and
  `VAULT_CLUSTER_INTERFACE` (setting `VAULT_REDIRECT_ADDR` to
  `http://<address>:8200` and `VAULT_CLUSTER_ADDR` to
  `https://<address>:8201`), `SKIP_CHOWN`.
- **Ports:** 8200. Volumes `/vault/logs` and `/vault/file`.
- **Users:** `vault` (uid 100, gid 1000, home `/home/vault`).
- **Differs:** macOS has no file capabilities, so Vault gets no `IPC_LOCK`
  capability and `SKIP_SETCAP` changes nothing; Vault locks memory as it
  is allowed to.

### fws/gitea

Mirrors `gitea/gitea` at 28 (28.4.0).

- **Entrypoint:** `/entry`; command `web`. The first start writes
  `/data/gitea/conf/app.ini`; every start applies `GITEA__SECTION__KEY`
  variables to it, as `environment-to-ini` does (`DEFAULT` names the root
  section, `_0X2E_` a dot and `_0X2D_` a hyphen). `web` runs
  `gitea web --config /data/gitea/conf/app.ini --work-path /data/gitea` as
  `git`; any other command runs as a `gitea` subcommand with the same
  `--config` and `--work-path`. Health: `GET /api/healthz` on 3000.
- **Defaults written:** HTTP on 0.0.0.0:3000 with `ROOT_URL`
  `http://localhost:3000/`, the built-in SSH server on 22, LFS, SQLite at
  `/data/gitea/gitea.db`, repositories in `/data/git/repositories`, the log
  on the console, `INSTALL_LOCK=false` (the install page shows).
- **Ports:** 3000 (HTTP), 22 (SSH). Volume `/data`.
- **Users:** `git` (1000:1000, home `/data/git`).
- **Differs:** `git` and `bash` (for repository hooks) are the image's;
  `USER_UID` and `USER_GID` are not read.

### fws/nginx

Mirrors `nginx:1` (mainline).

- **Entrypoint:** `/entry`; command `nginx`; stop signal `SIGQUIT`. Health:
  `GET /` on port 80.
- **Behaviour:** it renders `/etc/nginx/templates/*.template` into
  `/etc/nginx/conf.d`, substituting `$NAME` and `${NAME}` where the
  variable is set, so nginx's own variables (`$host`, `$uri`) reach nginx.
  It runs nginx with a main configuration of its own: one worker process,
  256 worker connections, the error log on standard error, the access log
  on standard output, temporary paths under `/var/lib/nginx`,
  `/etc/nginx/conf.d/*.conf` included, and, where `conf.d` holds no
  `.conf`, a default server on port 80 serving `/usr/share/nginx/html`. A
  file at `/etc/nginx/nginx.conf` replaces that main configuration.
  Arguments after `nginx` in the command go to nginx (`nginx -T`). nginx
  runs in the foreground with `-g 'daemon off;'` unless those arguments
  give `-g` themselves.
- **Ports:** 80.
- **Users:** `nginx` (101:101), which nothing selects.
- **Differs:** `NGINX_ENVSUBST_*` variables and `/docker-entrypoint.d`
  scripts are not read. A command other than `nginx` fails with `unknown
  command`.

### fws/toxiproxy

Mirrors Toxiproxy's image at 2.13 (2.13.0). No entry program.

- **Entrypoint:** `/toxiproxy`; command `-host=0.0.0.0`; `/toxiproxy-cli`
  beside it.
- **Environment:** `LOG_LEVEL=info`, and no `PATH` or `TZ` of its own.
- **Ports:** 8474 (API).

### fws/dind

Mirrors `docker:29-dind`, with Docker's 29.10.0 CLI.

- **Entrypoint:** `dockerd-entrypoint.sh` (also at `docker-entrypoint.sh`,
  where it does what the CLI image's script does). With no arguments or
  options alone, it runs `dockerd` on `unix:///var/run/docker.sock` (or
  `DOCKER_HOST`'s `unix://` socket) and TCP: with `DOCKER_TLS_CERTDIR`
  set, it generates or keeps a CA and makes server and client
  certificates under it (`DOCKER_TLS_SAN` adds names), and serves 2376
  with `--tlsverify`; with it empty, 2375 without TLS. Any other command
  runs with `DOCKER_HOST` set for the daemon, through the `docker` CLI when
  it names a docker command.
- **Environment:** `DOCKER_TLS_CERTDIR=/certs`, Debian's full `PATH`.
- **Ports:** 2375, 2376. Volume `/var/lib/docker`.
- **Requires:** the container MUST run `--privileged`: its `dockerd`
  serves the API of an engine fwsd nests in the container, whose socket
  `FWS_ENGINE` names. That `dockerd` takes `-H`/`--host`, `--tls`,
  `--tlsverify`, `--tlscacert`, `--tlscert`, `--tlskey`, `-D`/`--debug` and
  `-p`/`--pidfile`, and refuses any other flag.
- **Contents:** the `docker` CLI in `/usr/local/bin` with the buildx and
  Compose plugins in `/usr/local/libexec/docker/cli-plugins`, and the
  `podman` client; the socket serves Podman's API too.
- **Differs:** no iptables or rootlesskit steps; a non-root start fails as
  the official rootless check does.

### fws/ryuk

Mirrors `testcontainers/ryuk:0.14.0`, built from its release's commit.

- **Command:** `/bin/ryuk`; port 8080; `PATH=/bin`, `TZ=UTC`. Ryuk reads
  its own `RYUK_` variables.
- **Differs:** built with `fws-mkimage` alone: `/bin/ryuk` and `/bin/nc`,
  no shell and no `/etc/passwd`.

### fws/toolbox

The test suite's own image, mirroring none.

- **Entrypoint:** `/toolbox`; command `serve`, an HTTP counter backed by
  its stack's Redis on port 8080; other subcommands are probes that exit 0
  where the system allowed an operation and 1 where it refused. `registry
  ADDR` serves an image registry that keeps what is pushed to it in memory,
  listening at `ADDR`, as a kind cluster's local registry. Health:
  `/toolbox health`.
- **Differs:** `/toolbox` alone: no shell, `nc` or `/etc/passwd`.

### fws/kube-apiserver, fws/kube-controller-manager, fws/kube-scheduler, fws/kubectl

Mirror Kubernetes' release images at 1.37 (Kubernetes 1.37.2), built for
macOS with cgo off.

- **Entrypoint:** `/go-runner`; no command: the container's command is
  the binary and its flags (`kube-apiserver --etcd-servers=...`), with
  go-runner's own `--log-file` and `--also-stdout` before it. The binary is
  in `/usr/local/bin`.
- **Environment:** Debian's full `PATH`, `SSL_CERT_FILE` at the CA bundle.
  Working directory `/`.
- **Ports:** 6443 (API server), 10257 (controller manager), 10259
  (scheduler); `fws/kubectl` exposes none.
- **Users:** `nonroot` (65532:65532, home `/home/nonroot`), which no
  `User` selects.
- **Requires:** the API server advertises an address of its interfaces,
  which a network with a subnet of its own gives
  ([LIMITATIONS](../LIMITATIONS.md#networking)).
- **Differs:** a shell and `nc` where upstream's images are distroless; no
  kube-proxy or CoreDNS image.

### fws/kubelet

Kubernetes 1.37.2's kubelet with its darwin parts
(`images/kubernetes/kubelet-darwin.patch`).

- **Entrypoint:** `/usr/local/bin/kubelet`; `/usr/local/bin/fws-kube-proxy`
  beside it, which keeps the node engine's services to the cluster's.
- **Environment:** Debian's full `PATH`, `SSL_CERT_FILE`.
- **Ports:** 10250. Directories `/etc/kubernetes/manifests`,
  `/var/lib/kubelet`, `/var/log/pods`.
- **Requires:** the container MUST run `--privileged`, and the kubelet
  MUST be given the engine nested in it as its runtime
  (`--container-runtime-endpoint=unix://$FWS_ENGINE`) and the flags of a
  node without cgroups, as the end-to-end test gives them:
  `--cgroups-per-qos=false --enforce-node-allocatable=
  --fail-swap-on=false`.
- **Differs:** the node's capacity and use come from macOS's kernel; there
  are no cgroups.

### fws/kind-node

Mirrors kind's `kindest/node` at 1.37 (Kubernetes 1.37.2).

- **Entrypoint:** `/usr/local/bin/entrypoint`, which does what systemd and
  its units do in kind's node: it tells kind the node is up, loads the
  control plane's images (`/kind/images/*.tar`, named in
  `/kind/images.txt`) into the node's engine under the names kubeadm gives
  them, and keeps the kubelet running with `fws-kube-proxy` beside it.
- **Environment:** Debian's full `PATH`, `SSL_CERT_FILE`.
- **Ports:** 6443, 10250.
- **Requires:** the container MUST run `--privileged`, which kind's nodes
  are; without it the entry program exits with `the node needs its own
  engine`.
- **Contents:** `kubeadm` (the entry program, running upstream's at
  `/usr/local/libexec/fws/kubeadm` without the kube-proxy and CoreDNS
  addons), `kubectl`, `kubelet`, `fws-kube-proxy`; stand-ins for `cp`
  (copying standard input itself), `ctr`, `containerd` and `journalctl`;
  upstream's `crictl` against the node engine's CRI; `/kind/version` and
  `/kind/manifests`; coreutils, GNU tar and bash, which is `/bin/sh`.
- **Differs:** no containerd or systemd: their commands are the
  stand-ins above.

### fws/k3s

Mirrors `rancher/k3s` at 1.37, from `fws/kind-node`'s parts.

- **Entrypoint:** `/bin/k3s`; command `server`, which brings up a
  single-node cluster with kubeadm on the node's address and writes k3s's
  kubeconfig; `k3s kubectl` (with `KUBECONFIG` defaulting to it), `k3s ctr`,
  `k3s crictl` and `k3s --version` run against it.
- **Flags and variables read:** `--node-name` (`K3S_NODE_NAME`, default the
  hostname), `--tls-san`, `--cluster-cidr` (default `10.42.0.0/16`),
  `--service-cidr` (default `10.43.0.0/16`), `--write-kubeconfig`/`-o`
  (`K3S_KUBECONFIG_OUTPUT`, default `/etc/rancher/k3s/k3s.yaml`),
  `--write-kubeconfig-mode` (`K3S_KUBECONFIG_MODE`, default `0600`),
  `--https-listen-port`, which MUST be 6443, and `--cluster-init`, which
  the single server already is. `--disable` takes the components fws/k3s
  does not run (`traefik`, `servicelb`, `metrics-server`, `runtimes`,
  `helm-controller`), as Testcontainers' `--disable=traefik` names one,
  and `local-storage`, which leaves out the `local-path` StorageClass.
- **Refused:** any other `k3s server` flag, and `--disable` of any other
  component (`coredns`), fail the start with an error naming the flag.
- **Ports:** 6443.
- **Requires:** the container MUST run `--privileged`, on a network with a
  subnet (the default bridge network has one); `k3s agent` and other
  subcommands are refused.

### fws/pause

Kubernetes' pause program at 3.10.2, the infra container of a CRI pod
sandbox, built with clang from Kubernetes' `pause.c` against libSystem.

- **Entrypoint:** `/pause`; `User` `65535:65535`; working directory `/`;
  no environment.
