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
at the newest versions nixpkgs packages, except Kubernetes, still at
1.35.0.

## Roadmap

One pull request open at a time, large rather than granular. The roadmap
ends when its last gap is closed: fws then does what it sets out to do.

1. **Presentation gaps**, merged in
   [#79](https://github.com/e6qu/fun-with-sandboxes/pull/79): `PATH`,
   `HOME` and `docker top`'s commands read as the container's paths;
   tmpfs mounts and volumes take `uid` and `gid`.
2. **Remaining gaps** (#80, in progress), each removing a LIMITATIONS
   entry: Podman's `kube play`, `kube generate` and `kube down`; a DNS
   server a member of the stack runs, serving it; pod resource limits, the
   kubelet's `resources.limits` held as `--memory` and `--cpus`.

## Upkeep

Not on the roadmap, and done at any time, since none of it is needed to
show what fws does: newer catalog versions (the newest
`nixpkgs-26.05-darwin`, nixos-26.11 once released, and upstream releases
nixpkgs lacks built here, as MySQL and MariaDB already are in
`images/servers.nix`), and Kubernetes past 1.35 (1.36.3 in the pinned
channel, 1.37.2 in nixpkgs-unstable): the control-plane images, the
kubelet's darwin patch rebased, `fws/kind-node`, `fws/k3s` and a kind
release whose node image matches.
