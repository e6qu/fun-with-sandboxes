# fun-with-sandboxes — plan

A container engine for macOS that serves the Docker Engine API and Podman's
libpod API, so the stock `docker` CLI, Docker Compose, `podman`,
Testcontainers and kind drive it unchanged. Its containers run real macOS
binaries, each confined by Seatbelt, with the Docker primitives that
translate to macOS and a clear refusal of those that do not.

Mechanism: [`docs/DESIGN.md`](docs/DESIGN.md). What differs from Docker:
[`LIMITATIONS.md`](LIMITATIONS.md). How it got here:
[`docs/HISTORY.md`](docs/HISTORY.md).

## Constraints

- **No virtual machine, no root.** Everything runs as the invoking user: no
  hypervisor, `sudo`, root daemon, kernel extension or change to System
  Integrity Protection.
- **Isolation the kernel enforces.** Seatbelt confines every container
  process, and a test tries to break each property claimed.
- **Honour or refuse.** An option the engine cannot enforce fails with a
  Docker API error; none is accepted and dropped.
- **OCI throughout.** Images are layered OCI images in a content-addressed
  store; manifests and digests survive a pull, save, load and push byte for
  byte, and the engine speaks the distribution protocol to any registry.
- **Close to native.** A container costs what its processes cost; CI
  measures a process start and each interposed call against the native one.
- **Go, and C only inside a process start**, built for `darwin/arm64` alone.

Dockerfiles built `FROM` fws's base images are in scope: fws provides the
darwin base images (`fws/base`, the language and service images), and
`docker build`, buildx and Compose build on them whatever macOS can run.
Consuming Dockerfiles written for Linux is not a goal.

## Non-goals

- Linux binaries, in any form: unchanged images, a VM, system-call
  translation, a converter from Linux images, or Linux-specific Dockerfiles
  (`apt-get`, `apk`, `yum`) made to build.
- A security boundary against hostile code, Swarm, Intel Macs.
- Packaging for distribution: signed or notarized releases, installers, a
  `launchd` service. The engine is installed with `go install`.

## Where it stands

Docker's CLI, Compose, buildx and BuildKit, Testcontainers, Podman's
remote client, `podman-compose`, pods and `kube play`, the Dev Containers
CLI and VS Code's extension, graphical applications
(`--security-opt seatbelt=gui`), `crictl`, the kubelet, kind and k3s run
against the engine in CI on macOS 15 and 26. A catalog of 45 darwin images
at 56 tags is published to `ghcr.io/e6qu/fws-*` at the newest upstream
releases ([IMAGES](docs/IMAGES.md)); Kubernetes is at 1.37.2.

Seatbelt enforces a container's file reads and writes, its TCP
connections, its process listing and its IPC; its supervisor enforces its
memory, CPU, pids and disk limits, and its router and egress proxy its
network membership, bandwidth and allow lists. The container library
presents the rest: paths at their container values, ports, interfaces,
routes, users and the process tree. [LIMITATIONS](LIMITATIONS.md) lists
what is presented without being enforced.

## Roadmap

Four pull requests, in this order, each with the end-to-end tests that
proved its claims on macOS:

1. **Enforced isolation** (done,
   [#84](https://github.com/e6qu/fun-with-sandboxes/pull/84)): reads
   allow-listed as chroot would, TCP held to the container's own ports and
   its networks' members, and the process table its own.
2. **Limits macOS can enforce** (done,
   [#85](https://github.com/e6qu/fun-with-sandboxes/pull/85)): cpusets, a
   root size quota on an APFS disk image, and bandwidth.
3. **Networks: ingress, egress and routes** (done,
   [#86](https://github.com/e6qu/fun-with-sandboxes/pull/86)): egress allow
   lists per network and per container in the egress proxy, a source
   allow list on published ports, a routing table presented through the
   routing sysctl, and the bridge driver's options that translate
   (`enable_icc`, `enable_ip_masquerade`, `host_binding_ipv4`), with the
   rest refused.
4. **Images and the CLI** (done,
   [#86](https://github.com/e6qu/fun-with-sandboxes/pull/86)): Python
   3.11–3.15, Node.js 22, 24 and 26, and the Lambda runtimes AWS offers for
   them; `fws catalog`; and [IMAGES](docs/IMAGES.md), how images work.

The roadmap is finished. From here the work is upkeep (below) and
correctness: the static checks CI runs (`scripts/lint.sh`, the C
library's warnings and analyzer) stay clean, every bug found is fixed
where it is found, and compatibility with Compose, Testcontainers,
Podman, Kubernetes, kind and k3s is kept by the tests that run them.

## Upkeep

Done at any time, never ahead of the roadmap: the catalog, Kubernetes,
etcd and kind at their newest upstream releases. The images take
`nixpkgs-26.05-darwin`'s packages. Where it lags, they take
`images/catalog`'s instead: nixpkgs-unstable's packages, its recipes at
the newest release, or the release's own macOS build. `images/servers.nix`
builds MySQL and MariaDB, `images/etcd` builds etcd, and
`images/kubernetes` builds Kubernetes.
