# fun-with-sandboxes

A container engine for macOS that runs Docker Compose stacks side by side on
one Mac, without root and without a virtual machine. It serves the Docker
Engine API, so the stock `docker` CLI, `docker compose` and the Docker Go
client drive it unchanged.

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in [RFC 2119](https://www.rfc-editor.org/info/rfc2119).

It is for running a project's tests and services locally: a container
starts in milliseconds and costs only its processes, where Docker on a Mac
runs everything in a Linux VM. It is not a boundary for untrusted or
multi-tenant code.

Each container is a process tree on the host, confined by a generated
[Seatbelt](docs/SEATBELT.md) profile (the macOS sandbox) and rooted in an
APFS copy-on-write clone of its image. Each Compose project, a *stack*, gets
its own block of host ports and its own directory tree, so two stacks never
clash and never see each other.

- [`docs/USAGE.md`](docs/USAGE.md): installing and running the engine, every client, and worked use cases
- [`docs/IMAGES.md`](docs/IMAGES.md): the image catalog, the container contract, building and publishing images
- [`docs/IMAGE-AUTHORING.md`](docs/IMAGE-AUTHORING.md): writing images of your own, and adding or changing the catalog's
- [`LIMITATIONS.md`](LIMITATIONS.md): where it differs from Docker, and why
- [`PLAN.md`](PLAN.md): purpose, constraints, non-goals, roadmap
- [`docs/DESIGN.md`](docs/DESIGN.md): the mechanism
- [`docs/SEATBELT.md`](docs/SEATBELT.md): the macOS sandbox and how each container's profile confines it
- [`CONTRIBUTING.md`](CONTRIBUTING.md): setting up, testing and changing things
- [`docs/HISTORY.md`](docs/HISTORY.md): how it got here

## Run it

On a Mac with Apple silicon, Go and the Xcode Command Line Tools
(`xcode-select --install`), whose clang the engine builds its container
library with as it starts:

```bash
go install github.com/e6qu/fun-with-sandboxes/cmd/...@latest
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
`fws-shim`, which MUST sit beside it. It refuses to run as root, and keeps
its state in `~/.fws`. [USAGE](docs/USAGE.md#run-the-engine) lists its
flags: the state directory and socket, the host port range, the egress
proxy, BuildKit, live restore, on-demand image archives, the machine guard,
the QoS clamp and debug logging.

## Images

Containers run macOS programs, so images are `darwin/arm64`; Linux images
cannot run without a VM. fws comes with a catalog of images built from Nix
packages, each configured as the official image is: databases (Postgres,
MySQL, MariaDB, MongoDB, Redis, Valkey, ClickHouse, OpenSearch…), brokers
(Kafka, RabbitMQ, NATS), services (Vault, Consul, Gitea, nginx, Mailpit),
languages (Python 3.11–3.15, Node.js 22–26, Bun, Rust), the AWS Lambda
runtimes, Playwright, Docker in Docker, Kubernetes and kind, and
`fws/base`, a base to build `FROM`. CI publishes `fws/<name>:<tag>` as
`ghcr.io/e6qu/fws-<name>:<tag>`; pull one and tag it with its `fws/`
name, which Compose files and tests use:

```bash
docker pull ghcr.io/e6qu/fws-postgres:16 && docker tag ghcr.io/e6qu/fws-postgres:16 fws/postgres:16
```

`fws catalog` lists them. `docker build`, `buildx` and `docker compose
build` build `FROM` them on an embedded BuildKit with a darwin worker.
[IMAGES](docs/IMAGES.md) covers each image, and
[IMAGE-AUTHORING](docs/IMAGE-AUTHORING.md) writing images of your own.

## What a container can and cannot do

| | Allowed | Denied |
|---|---|---|
| Files | read and write its root and read-write volumes; read the macOS system tree, the shells, the time zone and the CA certificates | write anywhere else; read any other host path: `/Users`, `/private/etc`, `/usr/bin`, `/usr/local`, `/opt`, temp dirs, other stacks |
| Network | listen on its assigned loopback ports; connect to its stack's ports; reach the internet over HTTP(S) or SOCKS5 through its stack's egress proxy | listen on any other port or address; connect to anything else (a loopback port is refused, as in a network namespace); reach the host through the proxy |
| Processes | exec binaries in its root and the system shell; signal its own processes | exec other host binaries; signal or inspect other sandboxes' processes |
| Mach services | user lookup, notifications, logging, certificate trust evaluation | keychain, pasteboard, Launch Services, TCC and everything else |
| IPC | POSIX shared memory, semaphores and the Mach services it registers, under its own prefix; System V IPC, under keys of its own | other containers' names; all IPC with `--ipc none` |
| Everything else | none | Seatbelt denies by default: IOKit, Mach registration, preferences, kernel control sockets |

Each container runs under a supervisor that holds it to `--memory`,
`--cpus`, `--cpu-quota` and `--pids-limit` without cgroups, and its
processes start with its `--ulimit`s, `--cpu-shares` priority,
`--cpuset-cpus` placement and `--blkio-weight` disk I/O tier; `docker
update` changes them live. `--privileged` gives the container an engine of
its own (Docker in Docker, kind nodes), and `--user` is presented by the
container library. What the engine cannot honour without root or a VM
(added capabilities, host networking outside a nested engine, network
drivers other than bridge, devices) fails with an error; [LIMITATIONS.md](LIMITATIONS.md) lists each,
how every limit is enforced, and the few settings accepted without effect.

## What drives it

The engine serves Docker's API, Podman's (libpod) and Kubernetes' Container
Runtime Interface on one socket. CI runs each of these against it on
macOS; [USAGE](docs/USAGE.md) shows how to use each:

- the Docker CLI, Docker Compose (with `watch`), buildx and BuildKit;
- Testcontainers for Go, Java, Python, Node.js, Bun and Rust;
- Podman's remote client, `podman-compose`, pods and `podman kube play`;
- the Dev Containers CLI and VS Code's Dev Containers extension, with
  features, lifecycle commands, forwarded ports and Docker outside of
  Docker;
- Playwright, with Chromium, and Firefox in a container granted the
  user's session;
- graphical applications, Electron's and VS Code among them, with
  `--security-opt seatbelt=gui`;
- `crictl`, the kubelet, kind and k3s.

## Testcontainers

Testcontainers drives the engine through `DOCKER_HOST` like any Docker
daemon, with a few settings ([USAGE](docs/USAGE.md#testcontainers) has an
example for each library):

- Its reaper, Ryuk, ships as a Linux image; the catalog's
  `fws/ryuk:0.14.0` is a darwin build of the same release, tagged under
  the name each library runs.
- Catalog images take the place of Docker Hub's by name: pass
  `fws/postgres:16` where a module defaults to `postgres`, and, in
  Testcontainers for Java, `asCompatibleSubstituteFor("postgres")`.
- Testcontainers for Java sees a blank `OSType` in `/info`, which it
  accepts where it refuses `darwin`, and its startup checks, which run
  `alpine`, a Linux image, MUST be turned off with
  `TESTCONTAINERS_CHECKS_DISABLE=true`.

| Library | Release tested | Ryuk it runs |
| --- | --- | --- |
| Testcontainers for Go | 0.45 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Java | 2.0.5 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Node.js, under Node or Bun | 12.2.2 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Python | 4.15.0 | `testcontainers/ryuk:0.8.1` |
| testcontainers-rs | 0.27 | none: containers go when dropped |

```bash
docker tag fws/ryuk:0.14.0 testcontainers/ryuk:0.14.0
```

```bash
docker tag fws/ryuk:0.14.0 testcontainers/ryuk:0.8.1
```

## kind

[kind](https://kind.sigs.k8s.io)'s docker provider drives the engine, with
`fws/kind-node:1.37` as its node image:

```bash
kind create cluster --image fws/kind-node:1.37
```

Each node is a privileged container whose own engine runs its pods,
through the Container Runtime Interface the engine serves. The node's
entrypoint stands in for kindest/node's systemd: it loads the control
plane's images into the node's engine under the names kubeadm gives them,
and runs the kubelet on the configuration `kubeadm init` and `kubeadm
join` write, with `fws-kube-proxy` serving Services in place of
kube-proxy. Multi-node, IPv6 and dual-stack clusters,
`extraPortMappings`, `kind load`, `kind export logs` and `kind delete
cluster` work as with kindest/node ([USAGE](docs/USAGE.md#kind));
[LIMITATIONS](LIMITATIONS.md#kubernetes) lists what differs.

## Develop

```bash
go test ./...
```

On macOS the unit tests also run probes under the real sandbox.
[`CONTRIBUTING.md`](CONTRIBUTING.md) covers the setup, the end-to-end
suite and every check CI runs.
