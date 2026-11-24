# Images

How fws images work: what they are, where they come from, how to run,
build and publish them, and how the catalog is made.

## What an fws image is

An fws image is an ordinary OCI image whose platform is `darwin/arm64`. A
container runs its programs as processes on the Mac, so they must be
macOS binaries: nothing on macOS runs a Linux ELF binary without a
virtual machine, so images from Docker Hub do not run, and a `--platform`
other than `darwin/arm64` is refused.

Inside, an image is laid out as a Linux image is (`/bin`, `/etc`, `/usr`,
`/var`), with an `Entrypoint`, `Cmd`, `Env`, `ExposedPorts`, `Volumes` and
`Healthcheck` in its config that the engine honours as dockerd does. Two
things differ from a Linux image:

- **Everything it runs is in the image.** The Seatbelt profile lets a
  container read the macOS system tree (`/usr/lib`, `/System`) and run the
  system shells, nothing else of the host's, so an image carries every
  program and library its programs load, and loads them by relative
  paths. Each catalog image carries its own `/bin/sh` (`dash`): macOS's
  loader drops the container library from Apple's own shells, and with it
  from `system()` and `popen()`.
- **It follows the container contract** below: its programs find their
  paths and ports at their container values through the container
  library, which the engine loads into every process.

## Get the catalog

CI publishes every image of the catalog to GitHub's registry, on each push
to `main` whose tests pass, as `ghcr.io/e6qu/fws-<name>:<tag>` for
`fws/<name>:<tag>`:

```bash
docker pull ghcr.io/e6qu/fws-postgres:16
```

`fws catalog` lists the catalog: each image's tags, kind and what it
holds; `-published` prints where each tag is published, `-json` the whole
catalog, and arguments keep the images of a kind (`database`, `language`,
`lambda`, `kubernetes`…) or a name:

```bash
go install github.com/e6qu/fun-with-sandboxes/cmd/fws@latest
```

```bash
fws catalog database
```

Compose files and tests name the images as `fws/<name>:<tag>`; tag a
pulled image with that name, or build the archives locally (below) and
start the engine with `fwsd -image-archives DIR`, which loads an archive
the first time one of its images is asked for. The engine never
substitutes one image name for another: `postgres:16` is Docker Hub's
Linux image, and its pull fails.

## The catalog

Each image is configured as the official image is, through the same
variables, files and arguments:

| Image | Configured like the official image through |
|---|---|
| `fws/postgres:16`, `fws/postgres:18` | `POSTGRES_PASSWORD`, `POSTGRES_USER`, `POSTGRES_DB`, `POSTGRES_HOST_AUTH_METHOD`, `POSTGRES_INITDB_ARGS`, `/docker-entrypoint-initdb.d/*.sql`, `*.sql.gz` and `*.sh`, run against a server that listens on its socket alone; arguments to `postgres`, or another command, which runs as given |
| `fws/mariadb:11` | `MARIADB_ROOT_PASSWORD` (or the `ALLOW_EMPTY`/`RANDOM` forms), `MARIADB_DATABASE`, `MARIADB_USER`, `MARIADB_PASSWORD`, `MARIADB_ROOT_HOST`, their `MYSQL_` names, `/docker-entrypoint-initdb.d/*.sql`; a command other than the server runs as given |
| `fws/redis:8` | arguments to `redis-server`, with protected mode off unless a configuration file given first sets it, as the official entrypoint does |
| `fws/mysql:8.4` | `MYSQL_ROOT_PASSWORD` (or `MYSQL_ALLOW_EMPTY_PASSWORD`/`MYSQL_RANDOM_ROOT_PASSWORD`), `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_HOST`, `MYSQL_ONETIME_PASSWORD`, `/etc/mysql/conf.d/*.cnf`, `/docker-entrypoint-initdb.d/*.sql` and `*.sql.gz` |
| `fws/valkey:9` | arguments to `valkey-server`, or a `.conf` file; protected mode off unless that file sets it |
| `fws/memcached:1.6` | arguments to `memcached` |
| `fws/seaweedfs:4.54` | the subcommand and its flags, as `mini` (the default: master, volume, filer, S3 on 8333, WebDAV and the admin UI), `server`, `master`, `volume`, `filer`, `s3` or `shell`; `/data` |
| `fws/mongo:8.3` | `MONGO_INITDB_ROOT_USERNAME`, `MONGO_INITDB_ROOT_PASSWORD` (or their `_FILE` forms), `MONGO_INITDB_DATABASE`, `/docker-entrypoint-initdb.d/*.js` and `*.sh`, run against a server that listens on loopback alone; arguments to `mongod`, which binds every address unless they or its configuration name some; `mongosh` |
| `fws/etcd:3.7` | arguments to `etcd`, with `etcdctl` and `etcdutl`; clients on 2379, peers on 2380 |
| `fws/kube-apiserver:1.37`, `fws/kube-controller-manager:1.37`, `fws/kube-scheduler:1.37`, `fws/kubectl:1.37` | Kubernetes 1.37.2, the release kind's node image ships, laid out as its release images are: the command and its flags, run through `/go-runner` (its `--log-file` and `--also-stdout`), with the binary in `/usr/local/bin`; the API server on 6443, the controller manager on 10257, the scheduler on 10259 |
| `fws/kubelet:1.37` | Kubernetes 1.37.2's kubelet with its darwin parts (`images/kubernetes/kubelet-darwin.patch`): the node's capacity and use from macOS's kernel, and no cgroups; run privileged, it runs its pods on the engine nested in its container, through the CRI that engine serves at `$FWS_ENGINE` |
| `fws/k3s:1.37` | k3s's image, as Testcontainers' k3s module runs it, from `fws/kind-node`'s parts: `k3s server` brings up a single-node cluster with kubeadm on the node's address on the default bridge network and writes `/etc/rancher/k3s/k3s.yaml`; `k3s kubectl`, `ctr` and `crictl` run against it. `k3s.Run(ctx, "fws/k3s:1.37")` |
| `fws/kind-node:1.37` | kind's node image from the parts above: `kind create cluster --image fws/kind-node:1.37` runs kubeadm, the kubelet and `fws-kube-proxy` in each node, with the control plane's images loaded into the node's engine under the names kubeadm gives them (see [kind](../README.md#kind)) |
| `fws/pause:3.10.2` | Kubernetes' pause program, the infra container of a CRI pod sandbox |
| `fws/consul:2.1` | `CONSUL_LOCAL_CONFIG`, `CONSUL_BIND_INTERFACE`, `CONSUL_CLIENT_INTERFACE`; arguments to `consul` (default `agent -dev -client 0.0.0.0`); HTTP on 8500, DNS on 8600 |
| `fws/vault:2.2` | `VAULT_DEV_ROOT_TOKEN_ID`, `VAULT_DEV_LISTEN_ADDRESS`, `VAULT_LOCAL_CONFIG`, `VAULT_REDIRECT_INTERFACE`, `VAULT_CLUSTER_INTERFACE`; arguments to `vault` (default `server -dev`); port 8200 |
| `fws/kafka:4.4` | `KAFKA_` variables, written into `/etc/kafka/kafka.properties` as Confluent's images write them (`KAFKA_ADVERTISED_LISTENERS` becomes `advertised.listeners`), `CLUSTER_ID`, and `KAFKA_LOG4J_ROOT_LOGLEVEL` and `KAFKA_LOG4J_LOGGERS` for its log on standard output, through the `/etc/confluent/docker` scripts Testcontainers' module drives; one KRaft broker and controller by default, clients on 9092; Kafka's tools in `/usr/bin` |
| `fws/mailpit:1.34` | `MP_` variables and arguments to `mailpit`; SMTP on 1025, POP3 on 1110, the web UI and API on 8025 |
| `fws/meilisearch:1.57` | `MEILI_` variables, such as `MEILI_MASTER_KEY`, and arguments to `meilisearch`, which runs in `/meili_data`; the API on 7700 |
| `fws/qdrant:1.20` | `QDRANT__` variables over `/qdrant/config`, and `QDRANT_ALLOW_RECOVERY_MODE` as the official entrypoint reads it; storage and snapshots in `/qdrant`; REST and the dashboard on 6333, gRPC on 6334 |
| `fws/neo4j:2026.10` | `NEO4J_AUTH`, `NEO4J_PLUGINS` and `NEO4J_` settings, written into `/var/lib/neo4j/conf/neo4j.conf` as the official entrypoint writes them (`NEO4J_server_memory_heap_max__size` becomes `server.memory.heap.max_size`), and mounted `/conf`, `/plugins`, `/import`, `/licenses` and `/ssl`; data in `/data`, logs in `/logs`; HTTP on 7474, Bolt on 7687 |
| `fws/clickhouse:26.10` | `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`, `CLICKHOUSE_DB`, `/docker-entrypoint-initdb.d` and the other variables of the official image's own `entrypoint.sh`, which it runs; configuration in `/etc/clickhouse-server`, data in `/var/lib/clickhouse`; HTTP on 8123, native on 9000 |
| `fws/influxdb:2.10` | `DOCKER_INFLUXDB_INIT_*` setup and upgrade, `*_FILE` secrets and `/docker-entrypoint-initdb.d` scripts, as the official entrypoint handles them; the influx CLI configured in `/etc/influxdb2`, data in `/var/lib/influxdb2`; the API and UI on 8086 |
| `fws/opensearch:3.10` | the minimal distribution under the official image's entrypoint script, so dotted variables such as `discovery.type` become settings; runs as user 1000 from `/usr/share/opensearch`; REST on 9200 |
| `fws/dind:29` | Docker in Docker: run it `--privileged`, and its `dockerd` serves an engine of its own, which the official image's entrypoint arguments, `DOCKER_TLS_CERTDIR` and dockerd's `--host` and TLS flags configure; the Docker CLI with buildx and Compose; 2375, or 2376 with TLS |
| `fws/toxiproxy:2.13` | arguments to the server at `/toxiproxy`, with `/toxiproxy-cli` beside it; the API on 8474 |
| `fws/nats:2.15` | arguments to `nats-server`; `/nats-server.conf` serves clients on 4222, monitoring on 8222 and routes on 6222 |
| `fws/rabbitmq:4` | `RABBITMQ_DEFAULT_USER`, `RABBITMQ_DEFAULT_PASS`; AMQP on 5672, the Erlang port mapper on 4369 and distribution on 25672 |
| `fws/dynamodb-local:2` | arguments to DynamoDB Local (default `-sharedDb -dbPath /home/dynamodblocal/data`); port 8000 |
| `fws/gitea:28` | `GITEA__section__KEY` variables, written into `/data/gitea/conf/app.ini`; HTTP on 3000, SSH on 22 |
| `fws/nginx:1` | `/usr/share/nginx/html`, and `/etc/nginx/templates/*.template`, rendered into `conf.d` with environment variables substituted; the default server listens on 80 |
| `fws/python:3.11`, `fws/python:3.12`, `fws/python:3.13`, `fws/python:3.14`, `fws/python:3.15`, `fws/node:22`, `fws/node:24`, `fws/node:26`, `fws/bun:1` | the language and its package manager (`python3 -m ensurepip` for pip) |
| `fws/lambda/python:3.11`, `fws/lambda/python:3.12`, `fws/lambda/python:3.13`, `fws/lambda/python:3.14`, `fws/lambda/nodejs:22`, `fws/lambda/nodejs:24`, `fws/lambda/java:17` | the handler as the command; functions in `/var/task`, invoked at `POST /2015-03-31/functions/function/invocations` on port 8080 through AWS's Runtime Interface Emulator |
| `fws/playwright:1.65` | Node.js 24 with `playwright` and `@playwright/test`, so `npx playwright test` runs a mounted project's specs, and the browsers in `/ms-playwright` (`PLAYWRIGHT_BROWSERS_PATH`) that a project's own `@playwright/test@1.65.1` also finds: Chromium's headless shell, Chromium itself (`channel: 'chromium'`), Firefox, and ffmpeg for videos; no WebKit ([LIMITATIONS](../LIMITATIONS.md#images)). Firefox's own sandboxes are off (`MOZ_DISABLE_*_SANDBOX`), as Chromium runs with `--no-sandbox` |
| `fws/busybox:1` | a shell, `sh` by default, and the commands scripts run in BusyBox's image (`ls`, `cat`, `grep`, `sed`, `awk`, `find`, `tar`, `gzip`, `wget`, `nc -z`…) as separate programs, since BusyBox builds for Linux alone; `busybox CMD` runs them by name |
| `fws/base:1` | a base to build `FROM` in place of Alpine or Debian: `bash` by default, `fws/busybox`'s commands, `curl`, `git`, `jq`, `less`, and a CA bundle at `/etc/ssl/certs/ca-certificates.crt`; no package manager, as theirs install Linux binaries |
| `fws/ryuk:0.14.0` | Testcontainers' reaper (see [Testcontainers](../README.md#testcontainers)) |
| `fws/toolbox:1` | the test suite's small app and probe |

The Lambda images set `AWS_EXECUTION_ENV` as AWS's do
(`AWS_Lambda_python3.14`, `AWS_Lambda_nodejs24.x`).

Where Linux parts are missing an image differs from upstream's; LIMITATIONS
lists how ([Images](../LIMITATIONS.md#images)).

## The container contract

macOS gives an unprivileged process no private `/` and no private network
stack, so a container cannot own `/var/lib/postgresql/data` or port 5432.
The container library presents both, mapping the container's paths into its
root and its exposed and published ports onto host ports on loopback, and
the engine also tells the container where its things are:

| Variable | Meaning |
|---|---|
| `SANDBOX_ROOT` | the container's root directory on the host; image paths live under it |
| `SANDBOX_PORT_<n>` | the loopback host port of container port `<n>`, for a program that loads no library |
| `SANDBOX_<SERVICE>_PORT_<n>` | the host port of stack peer `<service>`'s port `<n>` |
| `SANDBOX_<SERVICE>_HOST` | the host to reach that peer on (`127.0.0.1`); through the container library a peer is reachable by name too |
| `SANDBOX_HOST` | `127.0.0.1`, the host a container's published ports are on |
| `SANDBOX_STACK` | the stack (Compose project) name |
| `SANDBOX_IPC_PREFIX` | the prefix POSIX shared memory and semaphore names start with under private IPC, which the container library adds |

A container's processes see `PATH` and `HOME` as the container's own. Containers with
a network also get `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` (SOCKS5, for
any TCP) and `NO_PROXY` pointing at their stack's egress proxy, with
credentials that name the container to it; `network_mode: none` and `internal: true`
networks get none, as in Docker. Volumes and bind
mounts appear at their destination inside the root. As Docker does, the engine writes
the container's `/etc/hosts` as it starts, with `localhost`, its
`--add-host` entries, its hostname at its member address and its legacy
links, and its `/etc/hostname`; the container library resolves the names
it lists before any other. A catalog image's entrypoint configures its
service as the official image's does, on its container ports and paths —
see `images/postgres/entry` and `internal/sandboxenv`.

## Build an image with a Dockerfile

`docker build`, `docker buildx build` and `docker compose build` run on an
embedded BuildKit with a darwin worker; `DOCKER_BUILDKIT=0` picks the
classic builder. Base the Dockerfile on a darwin image, most often
`fws/base:1`, which stands in for Alpine or Debian: `bash`, the everyday
commands (`ls`, `grep`, `sed`, `awk`, `find`, `tar`, `gzip`, `wget`…),
`curl`, `git`, `jq`, `less` and a CA bundle at
`/etc/ssl/certs/ca-certificates.crt`. A language image is the base for an
app in that language:

```dockerfile
FROM fws/python:3.14
COPY requirements.txt /app/
RUN python3 -m ensurepip && python3 -m pip install -r requirements.txt
COPY . /app
CMD ["python3", "main.py"]
```

Every instruction builds. A `RUN` step runs what its image holds, under
the same Seatbelt confinement as a container, with the internet through a
build-only egress proxy (none with `--network none`), so pip, npm, bun,
`curl` and `git` download as they do on Linux. There is no system package
manager: `apt-get`, `apk` and `yum` install Linux binaries and fail. Add a
program by downloading its macOS arm64 build, or by copying it in; it
must load its libraries from the image or macOS, by relative path
(`@loader_path`, `@executable_path`) rather than from `/opt/homebrew` or
`/nix/store`, which a container cannot read.

A native extension a package manager compiles (a Python wheel built from
source, a node-gyp addon) needs a compiler, which the images do not carry;
take packages that ship macOS arm64 builds, or build the extension on the
host and copy it in.

Multi-platform builds and `# syntax=` frontends other than Docker's own
are refused, as gateway frontends ship as Linux images.

## Run software of your own in an image

A program built for macOS runs unchanged when it reads its paths and ports
from its configuration: the container library presents `/` as the
container's root and container ports as its own. Where a program cannot
load the library (a static Go binary, one whose code signature forbids
injected libraries), its entrypoint reads the `SANDBOX_*` variables instead.
The catalog's entrypoints are small Go programs (`images/*/entry`) that do
what the official image's script does, through `internal/sandboxenv`:
`sandboxenv.Path` maps a container path into `SANDBOX_ROOT`, and
`sandboxenv.Port` gives the host port of a container port.

A script's `#!/usr/bin/env` line runs the program the container's `PATH`
names, as Apple's `env` cannot load the library.

## How the catalog is built

`scripts/build-images.sh DIR` builds every catalog image into an archive
in `DIR`, on macOS with Nix:

```bash
scripts/build-images.sh ./images-out
```

```bash
for t in ./images-out/*.tar; do docker load -i "$t"; done
```

1. **Packages from Nix.** Each image's programs are nixpkgs packages for
   `aarch64-darwin`, at a pinned commit of `nixpkgs-26.05-darwin`, so a
   rebuild makes the same images. Where that branch lags upstream,
   `images/catalog` builds the newest release: nixpkgs-unstable's package
   (pinned too), or its recipe at the release, or the vendor's own macOS
   build (MongoDB, Vault). `images/servers.nix` builds MySQL and MariaDB
   with their vendors' compilation comments, `images/kubernetes` builds
   Kubernetes, its darwin-patched kubelet and the pause program at their
   releases, and `images/etcd` etcd.
2. **A closure.** `nix-store -qR` lists every store path the packages
   reference: the image's whole dependency tree, and nothing else.
3. **`cmd/fws-mkimage`** turns the closure into an image:
   - copies each store path under `/nix/store` in the image;
   - rewrites every absolute store reference a Mach-O file loads through
     (`LC_LOAD_DYLIB`, `LC_RPATH`) to an `@loader_path`-relative one with
     `install_name_tool`, and re-signs the file ad hoc, as arm64 macOS
     refuses modified code otherwise;
   - turns absolute symlinks into relative ones and checks no Mach-O file
     still loads from the host's store, so the image is self-contained
     wherever its root lands (the profile denies the host's `/nix`, so a
     leftover reference fails the tests);
   - rewrites scripts' `#!` lines, which the kernel resolves on the host:
     an interpreter the image puts on `/bin` is named through
     `/usr/bin/env`, and a shell becomes the host's, with the store paths
     in its script prefixed by `$SANDBOX_ROOT`;
   - links each `-pkg`'s `bin/` into `/bin`, and adds `-file`s, `-link`s,
     the spec's `dirs` and `/etc/passwd` and `/etc/group` with the
     official image's users;
   - writes the config from the image's `images/<name>/image.json`, with
     `-tag` and `-env` over it;
   - writes an OCI image with a layer per store path, in sorted order,
     and a last layer with everything else. A store path's layer depends
     on that path alone, so images that share libraries share layers, in
     the store and in a registry.
4. **Entry programs.** The Go entrypoints, `images/toolbox`, the BusyBox
   multi-call stand-in and the other programs under `images/` are built
   with `go build` and added as files.

The archive is a `docker save` archive: an OCI image layout with Docker's
`manifest.json` beside it, which `docker load` and the engine's
`-image-archives` read.

`fws-mkimage -reuse` keeps an archive already at `-out` that was made from
the same inputs (spec, closure, files, flags), as the `.inputs` digest
beside it records them, so rebuilding a catalog rebuilds only what
changed. `scripts/build-images.sh --inputs` lists every file the build
reads, which CI keys its cache on, and `--prebuilt GROUP` builds the
packages no binary cache holds (the database servers, Kubernetes, Consul
and the `images/catalog` releases), which CI builds side by side and keeps
([CONTRIBUTING](../CONTRIBUTING.md#test)).

## Publish

`scripts/publish-images.sh DIR PREFIX` pushes each archive's OCI layout,
byte for byte, as `PREFIX/fws-<name>:<tag>`, so an image's digest is the
same in the archive, the registry and every engine that pulls it:

```bash
scripts/publish-images.sh ./images-out ghcr.io/you
```

CI publishes each pull request's catalog to a local registry and checks
every tag `fws catalog -published` lists is there as a darwin image; on
`main`, once the end-to-end suite passes, it publishes to
`ghcr.io/e6qu`.

## How the engine stores images

The engine keeps an OCI image layout under `~/.fws/images`: pulled,
loaded and built images keep their manifests, configs and layers byte for
byte, so digests and image IDs survive a save, load, push or pull, as in
Docker. Each layer is unpacked once; an image's root is its layers laid
over one another with APFS clones, and each container's root is a clone of
that, copy-on-write, made in constant time. [DESIGN §4](DESIGN.md#4-image-and-root-filesystem)
has the details.

## Add an image or a release

- **An image.** Add its spec and entry program under `images/<name>`, its
  build to `scripts/build-images.sh`, its row to the table above and its
  entry to `internal/catalog` (a unit test checks the two agree), and an
  end-to-end test in `tests/e2e/catalog_test.go` that drives it through
  the client its users would use.
- **A release.** Take the newest. An image takes
  `nixpkgs-26.05-darwin`'s package; where that lags, add it to
  `images/catalog`: nixpkgs-unstable's package when it has the release,
  else its recipe copied there at the release, or the release's own macOS
  build. GitHub's source archives hash as `git archive` of the tag lays
  the tree out.
