# fun-with-sandboxes

A container engine for macOS that runs Docker Compose stacks side by side on
one Mac, without root and without a virtual machine. It serves the Docker
Engine API, so the stock `docker` CLI, `docker compose` and the Docker Go
client drive it unchanged.

It is for running a project's tests and services locally: a container
starts in milliseconds and costs only its processes, where Docker on a Mac
runs everything in a Linux VM. It is not a boundary for untrusted or
multi-tenant code.

Each container is a process tree on the host, confined by a generated
[Seatbelt](docs/SEATBELT.md) profile (the macOS sandbox) and rooted in an
APFS copy-on-write clone of its image. Each Compose project, a *stack*, gets
its own block of host ports and its own directory tree, so two stacks never
clash and never see each other.

- [`PLAN.md`](PLAN.md): purpose, constraints, non-goals, roadmap
- [`docs/DESIGN.md`](docs/DESIGN.md): the mechanism
- [`docs/IMAGES.md`](docs/IMAGES.md): the image catalog, the container contract, building and publishing images
- [`docs/SEATBELT.md`](docs/SEATBELT.md): the macOS sandbox and how each container's profile confines it
- [`LIMITATIONS.md`](LIMITATIONS.md): where it differs from Docker, and why
- [`CONTRIBUTING.md`](CONTRIBUTING.md): setting up, testing and changing things
- [`docs/HISTORY.md`](docs/HISTORY.md): how it got here

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
`fws-shim`, which must sit beside it. As it starts it builds the container
library and spawn helper with clang from the Xcode Command Line Tools
(`xcode-select --install`). It refuses to run as root.

| Flag | Default | |
|---|---|---|
| `-root` | `~/.fws` | the engine's state directory |
| `-socket` | `<root>/docker.sock` | the socket the Docker, libpod and CRI APIs are served on |
| `-port-base`, `-port-limit`, `-block-size` | 20000, 30000, 100 | the host ports handed to stacks, in a block per stack |
| `-egress` | on | each stack's HTTP and SOCKS5 proxy to the internet |
| `-buildkit` | on | BuildKit for `docker build`, buildx and `docker compose build` |
| `-live-restore` | off | containers keep running while `fwsd` stops, and the next `fwsd` takes them back |
| `-image-archives DIR` | none | load an image from the archives in `DIR` (as `scripts/build-images.sh` or `docker save` writes them) the first time it is asked for |
| `-machine-guard` | on | while macOS reports critical memory pressure, or the state disk has under 2 GiB free, pause the container using the most of it, one at a time, and resume them once it is normal |
| `-qos-clamp` | `utility` | the QoS every container process runs at most at, so containers yield the CPU and disk to your own work: `utility`, `background`, `maintenance` or `none` |
| `-debug` | off | log each request with its time, and the steps of each create and start |

## Images

Containers run macOS programs, so images are `darwin/arm64`; Linux images
cannot run without a VM. fws comes with a catalog of images built from Nix
packages, each configured as the official image is: databases (Postgres,
MySQL, MariaDB, MongoDB, Redis, Valkey, ClickHouse, OpenSearch…), brokers
(Kafka, RabbitMQ, NATS), services (Vault, Consul, Gitea, nginx, Mailpit),
languages (Python 3.11–3.15, Node.js 22–26, Bun, Rust), the AWS Lambda
runtimes, Playwright, Docker in Docker, Kubernetes and kind, and
`fws/base`, a base to build `FROM`. CI publishes `fws/<name>:<tag>` as
`ghcr.io/e6qu/fws-<name>:<tag>`, and `fws catalog` lists them:

```bash
docker pull ghcr.io/e6qu/fws-postgres:16
```

```bash
go install github.com/e6qu/fun-with-sandboxes/cmd/fws@latest && fws catalog
```

`docker build`, `buildx` and `docker compose build` build `FROM` them on
an embedded BuildKit with a darwin worker. [`docs/IMAGES.md`](docs/IMAGES.md)
covers each image, the contract an image follows to run here, writing
Dockerfiles, and how the catalog is built and published.

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

### Dev Containers

The Dev Containers CLI (`devcontainer up`, `exec`) and VS Code's Dev
Containers extension drive the engine as they drive Docker: a
`devcontainer.json` on a darwin image, a Dockerfile or a Compose service
(with `runServices`), with features, every lifecycle command from
`initializeCommand` on the host to `postAttachCommand`, `containerUser`
and `remoteUser`, `containerEnv` and `remoteEnv`, `mounts`, the workspace
mount and `appPort`, and reopening and rebuilding the container. The
extension installs VS Code's macOS server in the container, with the
extensions and settings of `customizations.vscode`, forwards
`forwardPorts`, lends the container the host's Git credentials and SSH
agent, and stops the container when VS Code quits. Docker outside of
Docker is the engine's socket in `mounts` and a darwin Docker CLI in the
image, from `fws/dind`. The image needs the commands the tools run in it
(`sh`, `sleep`, `cat`, `mkdir`, `tar`, as `fws/base` has), and features
install darwin programs, not `apt-get` packages
([LIMITATIONS](LIMITATIONS.md#images)).

### Graphical applications

`--security-opt seatbelt=gui` also grants a container the user's login
session: the window server, pasteboard, Launch Services, fonts, input
methods, preferences, audio and the GPU, which a graphical application
such as an Electron one draws and listens with; VS Code itself runs in
one, with its extension host and integrated terminal. Its windows open on
the user's desktop and it reads what the user copies; the keychain stays
denied. A devcontainer asks for it with
`"runArgs": ["--security-opt", "seatbelt=gui"]`, a Compose service with
`security_opt`. Such a container runs as the engine's user
(`--user "$(id -u):$(id -g)"`): the session's services learn the caller
from the kernel and refuse a process that calls itself root.
Chromium-based applications take `--no-sandbox`
([LIMITATIONS](LIMITATIONS.md#isolation)).

## Testcontainers

Testcontainers drives the engine through `DOCKER_HOST` like any Docker
daemon:

```bash
export DOCKER_HOST=unix://$HOME/.fws/docker.sock
```

A reaper mounts the socket `DOCKER_HOST` names, unless
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` names another. Containers' ports
are on loopback, so the default host, `localhost`, reaches them.

The reaper, Ryuk, ships as a Linux image; the catalog's `fws/ryuk:0.14.0`
is a darwin build of the same release. Tag it under the name each library
runs, and the reaper removes what a test process leaves behind:

| Library | Release tested | Ryuk it runs |
| --- | --- | --- |
| Testcontainers for Go | 0.45 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Java | 2.0.5 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Node.js, under Node or Bun | 12.2.2 | `testcontainers/ryuk:0.14.0` |
| Testcontainers for Python | 4.15.0 | `testcontainers/ryuk:0.8.1` |
| testcontainers-rs | 0.27 | none: containers go when dropped |

```bash
docker tag fws/ryuk:0.14.0 testcontainers/ryuk:0.14.0
docker tag fws/ryuk:0.14.0 testcontainers/ryuk:0.8.1
```

Catalog images take the place of Docker Hub's by name: pass
`fws/postgres:16` where a module defaults to `postgres`. Testcontainers
for Java checks that a substitute is compatible, so name what it stands
for:

```java
var pg = new PostgreSQLContainer(
    DockerImageName.parse("fws/postgres:16").asCompatibleSubstituteFor("postgres"));
```

Testcontainers for Java refuses an engine whose `OSType` is not `linux`,
so the engine answers its `/info` with a blank one, which it accepts. Its
file-mounting startup check runs `alpine`, a Linux image: turn the checks
off with `TESTCONTAINERS_CHECKS_DISABLE=true`, or `checks.disable=true` in
`~/.testcontainers.properties`. It runs Ryuk privileged, which here gives
the reaper an engine of its own that it does not use;
`TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED=false` saves starting it.

## Podman

The engine serves Podman's API, libpod, on the same socket, so Podman's
remote client and `podman-compose` drive it as they drive Podman:

```bash
export CONTAINER_HOST=unix://$HOME/.fws/docker.sock
podman --remote run --rm fws/python:3.14 python3 -c 'print("hi")'
```

A pod's infra container runs `fws/pause`, and the pod's containers join
its network, IPC and, where shared, PID namespace; `--infra=false
--share=`, as `podman-compose` creates pods, makes one that shares
nothing. `podman kube play` runs Kubernetes YAML, `podman kube generate`
writes a pod back, and `podman kube down` removes it
([LIMITATIONS](LIMITATIONS.md#api-surface) lists what `kube play` runs):

```bash
podman --remote kube play pod.yaml
```

## Kubernetes' Container Runtime Interface

The socket serves the CRI too, as gRPC over HTTP/2, so the kubelet and
`crictl` use the engine as they use containerd:

```bash
crictl --runtime-endpoint unix://$HOME/.fws/docker.sock pods
```

A pod sandbox is an `fws/pause:3.10.2` container holding the pod's network
and published ports, which the pod's containers join, as cri-dockerd maps
CRI onto Docker's. Pods join the engine's pod network, `fws-pods`, on the
node's pod CIDR (IPv4, IPv6 or dual-stack), and reach the pods of other
nodes sharing a network with theirs; a `hostNetwork` pod, as kubeadm's
static pods are, shares its node container's network. `fws-kube-proxy`, in
`fws/kubelet`'s image, is the node's kube-proxy: it keeps the engine's
Services on the pod network matching the cluster's, reachable at their
cluster IPs, by their DNS names and, for NodePort Services, at the node's
ports. The engine writes container output to the kubelet's log files in
CRI's format, and serves exec, attach and port-forward streams as
containerd does.

## kind

[kind](https://kind.sigs.k8s.io)'s docker provider drives the engine, with
`fws/kind-node:1.37` as its node image:

```bash
export DOCKER_HOST=unix://$HOME/.fws/docker.sock
kind create cluster --image fws/kind-node:1.37
kubectl get nodes
```

Each node is a privileged container whose engine runs its pods. The
node's entrypoint stands in for kindest/node's systemd: it loads the
control plane's images into the node's engine under the names kubeadm
gives them, and runs the kubelet on the configuration `kubeadm init` and
`kubeadm join` write, with `fws-kube-proxy` beside it. kubeadm skips its
kube-proxy and CoreDNS addons; kind's CNI manifest is a binding that lets
`fws-kube-proxy` read Services, and its storage class uses the controller
manager's host-path provisioner. Multi-node clusters, the API server on
the loopback port kind publishes, `extraPortMappings`, `kind load
docker-image`, `kind export logs` and `kind delete cluster` work as with
kindest/node: the node's `ctr`, `containerd` and `journalctl` stand in for
those kind runs, and its `crictl` is upstream's.
[LIMITATIONS](LIMITATIONS.md#kubernetes) lists what differs.

## Develop

```bash
go test ./...
```

On macOS the unit tests also run probes under the real sandbox.
[`CONTRIBUTING.md`](CONTRIBUTING.md) covers the setup, the end-to-end
suite and every check CI runs.
