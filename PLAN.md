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
against the engine in CI on macOS 15 and 26. That includes bridge networks
with IPv4, IPv6 and dual-stack subnets, published ports, `--network
container:` sidecars, internal networks and the egress proxy. A catalog of
46 darwin images is published to `ghcr.io/e6qu/fws-*` at the newest
upstream releases. It includes Python, Node.js and the AWS Lambda runtimes
for both. Kubernetes is at 1.37.2.

What a container sees is presented by the container library, and Seatbelt
enforces part of it. The roadmap moves the rest from presented to
enforced, wherever an unprivileged process on macOS can enforce it.

## Roadmap

Four pull requests to the finish line, in this order. Each one ships with
the e2e tests that prove its claims on macOS.

1. **Enforced isolation.** A container behaves as if under chroot, even
   for programs that load no library:
   - It reads only its root, its volumes, the system tree and the few host
     files a root would hold. Seatbelt allow-lists reads instead of
     denying a list of roots (in progress).
   - A container's ports are its own. Every listener sits on a Unix socket
     in the container's port directory. Seatbelt lets a container connect
     only to the sockets of the networks it is a member of, and the router
     alone holds the published host ports. This makes network membership
     and internal ports enforced, not presented.
   - Its process listing is its own. Seatbelt denies process information
     about other sandboxes, and the `kern.proc` listing is narrowed where
     the kernel allows it.
2. **Limits macOS can enforce.**
   - CPU placement and priority: `--cpuset-cpus` maps to performance or
     efficiency cores through QoS, with per-container QoS options.
   - A disk quota: `--storage-opt size` puts the root on a sparse APFS
     disk image the user mounts.
   - Bandwidth: a rate per container in the router and egress proxy.
3. **Networks: ingress, egress and routes.**
   - Egress policy per network and per container: allow-listed hosts and
     ports in the egress proxy, denied elsewhere by Seatbelt.
   - Ingress through published ports, with a source allow-list.
   - Presented routes in `docker network inspect` and the routing sysctl
     for a network's gateways and subnets.
   - The `docker network` options that remain, where they translate.
4. **Images and the CLI.**
   - More versions of the Python, Node.js and Lambda images: the
     supported releases each upstream maintains.
   - An `fws` command that lists the catalog's images, their versions and
     their tags (`fws catalog`).

Done when all four have merged and `main` is green through `publish`.
Compatibility with Compose, Testcontainers, Podman, Kubernetes, kind and
k3s is kept by the tests that already run them; gaps they turn up go into
the open pull request.

## Upkeep

Done at any time, never ahead of the roadmap: the catalog, Kubernetes,
etcd and kind at their newest upstream releases. The images take
`nixpkgs-26.05-darwin`'s packages. Where it lags, they take
`images/catalog`'s instead: nixpkgs-unstable's packages, its recipes at
the newest release, or the release's own macOS build. `images/servers.nix`
builds MySQL and MariaDB, `images/etcd` builds etcd, and
`images/kubernetes` builds Kubernetes.
