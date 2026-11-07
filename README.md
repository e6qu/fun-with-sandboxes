# fun-with-sandboxes

A container engine for macOS that runs Docker Compose stacks side by side on
one Mac — without root and without a virtual machine. It serves the Docker
Engine API, so the stock `docker` CLI, `docker compose` and the Docker Go
client drive it unchanged.

Each container is a process tree on the host, confined by a generated
**[Seatbelt](docs/SEATBELT.md)** profile (the macOS sandbox) and rooted in an **APFS copy-on-write clone** of its
image. Each Compose project — a *stack* — gets its own block of host ports and
its own directory tree, so two stacks never clash and never see each other.

- [`PLAN.md`](PLAN.md) — constraints, isolation model, milestone
- [`docs/DESIGN.md`](docs/DESIGN.md) — the full mechanism
- [`docs/SEATBELT.md`](docs/SEATBELT.md) — the macOS sandbox, a tutorial, and how each container's profile confines it
- [`LIMITATIONS.md`](LIMITATIONS.md) — what it cannot do, and why
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — setting up, testing, and changing things

## Run it

```bash
go install github.com/e6qu/fun-with-sandboxes/cmd/fwsd@latest github.com/e6qu/fun-with-sandboxes/cmd/fws-shim@latest
```

```bash
fwsd
```

```bash
export DOCKER_HOST=unix://$HOME/.fws/docker.sock
```

```bash
docker compose up -d --wait
```

`fwsd` runs each container's supervisor, and the processes it starts, from
`fws-shim`, which must sit beside it. It builds its container library with
clang from the Xcode Command Line Tools (`xcode-select --install`). `fwsd` refuses to run as root. State lives in `~/.fws` (`-root`), stacks get
host ports from 20000–29999 in blocks of 100 (`-port-base`, `-port-limit`,
`-block-size`). With `-live-restore` containers keep running while `fwsd`
stops, for an upgrade say, and the next `fwsd` takes them back.

## Images

Images are macOS-native (`darwin/arm64` or `darwin/amd64`). Linux images
cannot run without a VM. `scripts/build-images.sh` builds the catalog from
Nix packages into archives for `docker load`, and `scripts/publish-images.sh`
pushes them to a registry the engine can pull from. MySQL and MariaDB are
built from source, with the compilation comment their vendors' builds
carry (`images/servers.nix`), so the first build compiles them, as it does
Consul, which no binary cache holds:

| Image | Configured like the official image through |
|---|---|
| `fws/postgres:16`, `fws/postgres:18` | `POSTGRES_PASSWORD`, `POSTGRES_USER`, `POSTGRES_DB`, `POSTGRES_HOST_AUTH_METHOD`, `POSTGRES_INITDB_ARGS`, `/docker-entrypoint-initdb.d/*.sql`, `*.sql.gz` and `*.sh`, run against a server that listens on its socket alone; arguments to `postgres` |
| `fws/mariadb:11` | `MARIADB_ROOT_PASSWORD` (or the `ALLOW_EMPTY`/`RANDOM` forms), `MARIADB_DATABASE`, `MARIADB_USER`, `MARIADB_PASSWORD`, `MARIADB_ROOT_HOST`, their `MYSQL_` names, `/docker-entrypoint-initdb.d/*.sql` |
| `fws/redis:8` | arguments to `redis-server` |
| `fws/mysql:8.4` | `MYSQL_ROOT_PASSWORD` (or `MYSQL_ALLOW_EMPTY_PASSWORD`/`MYSQL_RANDOM_ROOT_PASSWORD`), `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_HOST`, `MYSQL_ONETIME_PASSWORD`, `/etc/mysql/conf.d/*.cnf`, `/docker-entrypoint-initdb.d/*.sql` and `*.sql.gz` |
| `fws/valkey:9` | arguments to `valkey-server`, or a `.conf` file |
| `fws/memcached:1.6` | arguments to `memcached` |
| `fws/seaweedfs:4.36` | the subcommand and its flags, as `mini` (the default: master, volume, filer, S3 on 8333, WebDAV and the admin UI), `server`, `master`, `volume`, `filer`, `s3` or `shell`; `/data` |
| `fws/mongo:8.2` | `MONGO_INITDB_ROOT_USERNAME`, `MONGO_INITDB_ROOT_PASSWORD` (or their `_FILE` forms), `MONGO_INITDB_DATABASE`, `/docker-entrypoint-initdb.d/*.js` and `*.sh`, run against a server that listens on loopback alone; arguments to `mongod`, which binds every address unless they or its configuration name some; `mongosh` |
| `fws/etcd:3.6` | arguments to `etcd`, with `etcdctl` and `etcdutl`; clients on 2379, peers on 2380 |
| `fws/consul:2.1` | `CONSUL_LOCAL_CONFIG`, `CONSUL_BIND_INTERFACE`, `CONSUL_CLIENT_INTERFACE`; arguments to `consul` (default `agent -dev -client 0.0.0.0`); HTTP on 8500, DNS on 8600 |
| `fws/vault:2.0` | `VAULT_DEV_ROOT_TOKEN_ID`, `VAULT_DEV_LISTEN_ADDRESS`, `VAULT_LOCAL_CONFIG`, `VAULT_REDIRECT_INTERFACE`, `VAULT_CLUSTER_INTERFACE`; arguments to `vault` (default `server -dev`); port 8200 |
| `fws/kafka:4.2` | `KAFKA_` variables, written into `/etc/kafka/kafka.properties` as Confluent's images write them (`KAFKA_ADVERTISED_LISTENERS` becomes `advertised.listeners`), `CLUSTER_ID`, and `KAFKA_LOG4J_ROOT_LOGLEVEL` and `KAFKA_LOG4J_LOGGERS` for its log on standard output, through the `/etc/confluent/docker` scripts Testcontainers' module drives; one KRaft broker and controller by default, clients on 9092; Kafka's tools in `/usr/bin` |
| `fws/mailpit:1.32` | `MP_` variables and arguments to `mailpit`; SMTP on 1025, POP3 on 1110, the web UI and API on 8025 |
| `fws/nats:2.14` | arguments to `nats-server`; `/nats-server.conf` serves clients on 4222, monitoring on 8222 and routes on 6222 |
| `fws/rabbitmq:4` | `RABBITMQ_DEFAULT_USER`, `RABBITMQ_DEFAULT_PASS`; AMQP on 5672, the Erlang port mapper on 4369 and distribution on 25672 |
| `fws/dynamodb-local:2` | arguments to DynamoDB Local (default `-sharedDb -dbPath /home/dynamodblocal/data`); port 8000 |
| `fws/gitea:1` | `GITEA__section__KEY` variables, written into `/data/gitea/conf/app.ini`; HTTP on 3000, SSH on 22 |
| `fws/nginx:1` | `/usr/share/nginx/html`, and `/etc/nginx/templates/*.template`, rendered into `conf.d` with environment variables substituted; the default server listens on 80 |
| `fws/python:3.14`, `fws/node:26`, `fws/bun:1` | the language and its package manager (`python3 -m ensurepip` for pip) |
| `fws/lambda/python:3.14`, `fws/lambda/nodejs:24`, `fws/lambda/java:17` | the handler as the command; functions in `/var/task`, invoked at `POST /2015-03-31/functions/function/invocations` on port 8080 through AWS's Runtime Interface Emulator |
| `fws/playwright:1.59` | Node.js 24 with `playwright` and its Chromium headless shell |
| `fws/toolbox:1` | the test suite's small app and probe |

```bash
scripts/build-images.sh ./images-out
```

```bash
for t in ./images-out/*.tar; do docker load -i "$t"; done
```

`cmd/fws-mkimage` turns any Nix closure into an image: it rewrites the
closure's absolute `/nix/store` library paths and symlinks relative to each
file, so the image is self-contained wherever its root lands.

### Building images

`docker build`, `docker buildx build` and `docker compose build` run on an
embedded BuildKit with a darwin worker. Base a Dockerfile on a darwin image;
`RUN` steps run under the same Seatbelt confinement as containers, with
internet through a build-only egress proxy:

```bash
docker buildx build --load -t my/app:1 .
```

The classic builder stays available with `DOCKER_BUILDKIT=0`.

Images are layered OCI images. The catalog images carry a layer per Nix
store path, so images that share libraries share those layers; builds add a
layer per filesystem step on top of their base's layers, and `docker commit`
adds one. `docker save` writes an OCI image layout that also carries Docker's
`manifest.json`, `docker load` reads either, and `docker push` sends an
image's manifest and layers exactly as stored, so its digest survives a push
and a pull.

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
| `SANDBOX_STACK` | the stack (Compose project) name |
| `SANDBOX_IPC_PREFIX` | the prefix POSIX shared memory and semaphore names start with under private IPC, which the container library adds |

`PATH` entries in the image config are mapped into the root. Containers with
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

## What a container can and cannot do

| | Allowed | Denied |
|---|---|---|
| Files | read and write its root and read-write volumes; read the macOS system tree | write anywhere else; read `/Users`, `/Volumes`, `/Library`, `/opt`, `/usr/local`, `/nix`, temp dirs, other stacks |
| Network | listen on its assigned loopback ports; connect to its stack's ports; reach the internet over HTTP(S) or SOCKS5 through its stack's egress proxy | listen on any other port or address; connect to anything else; reach the host through the proxy |
| Processes | exec binaries in its root and the system shell; signal its own processes | exec other host binaries; signal or inspect other sandboxes' processes |
| Mach services | user lookup, notifications, logging, certificate trust evaluation | keychain, pasteboard, Launch Services, TCC and everything else |
| IPC | POSIX shared memory, semaphores and the Mach services it registers, under its own prefix; System V IPC, under keys of its own | other containers' names; all IPC with `--ipc none` |
| Everything else | — | Seatbelt denies by default: IOKit, Mach registration, preferences, kernel control sockets |

Each container runs under a supervisor that holds it to `--memory`,
`--cpus`, `--cpu-quota` and `--pids-limit` without cgroups, and its
processes start with its `--ulimit`s, `--cpu-shares` priority and
`--blkio-weight` disk I/O tier; `docker update` changes them live (see
[LIMITATIONS.md](LIMITATIONS.md) for how each is enforced). Requests the
engine cannot honour without root — cpusets, user switching, privileged
mode, added capabilities, host networking, other network drivers — fail with
an error rather than being ignored.

## Testcontainers

Testcontainers drives the engine through `DOCKER_HOST` like any Docker
daemon. Its reaper, Ryuk, ships as a Linux image; the catalog's
`fws/ryuk:0.14.0` is a darwin build of the same release. Load it and give it
the name Testcontainers runs, and the reaper removes what a test process
leaves behind when it exits:

```bash
docker tag fws/ryuk:0.14.0 testcontainers/ryuk:0.14.0
```

Testcontainers for Go 0.44 runs Ryuk 0.14.0; other Testcontainers releases
name other Ryuk versions.

## Develop

```bash
go test ./...
```

On macOS the Seatbelt package also runs a probe under the real sandbox. The
end-to-end suite needs the image archives, the Docker CLI and Docker
Compose; [`CONTRIBUTING.md`](CONTRIBUTING.md) covers the setup and every
check CI runs:

```bash
cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e ./...
```
