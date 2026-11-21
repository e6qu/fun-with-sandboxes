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

Feature-complete for its purpose. Docker's CLI, Compose, buildx and
BuildKit, Testcontainers, Podman's remote client and `podman-compose`, the
Dev Containers CLI and VS Code's Dev Containers extension, graphical
applications (`--security-opt seatbelt=gui`), `crictl`, the kubelet and
kind, IPv4, IPv6 and dual-stack, run against the engine in CI on macOS 15
and 26. A catalog of 46 darwin images is published to `ghcr.io/e6qu/fws-*`,
at the newest upstream releases, and Kubernetes at 1.37.2.

## Roadmap

Complete: its last gaps closed in
[#80](https://github.com/e6qu/fun-with-sandboxes/pull/80). fws does what it
sets out to do.

## Upkeep

Done at any time, since none of it is needed to show what fws does: the
catalog and Kubernetes at their newest upstream releases. The images take
`nixpkgs-26.05-darwin`'s packages (nixos-26.11's once released), and,
where it lags, `images/catalog`'s: nixpkgs-unstable's packages, its
recipes at the newest release, or the release's own macOS build, as for
ClickHouse; `images/servers.nix` builds MySQL and MariaDB and
`images/etcd` etcd, at their newest releases, and `images/kubernetes`
Kubernetes. Kubernetes 1.37.2, etcd 3.7.4 and kind 0.33.0 merged in
[#81](https://github.com/e6qu/fun-with-sandboxes/pull/81); the catalog at
its newest releases is in progress.
