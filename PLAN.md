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

One pull request open at a time, large rather than granular.

1. **Presentation gaps** (#79, in progress): `PATH`, `HOME` and `docker
   top`'s commands read as the container's paths; tmpfs mounts and volumes
   take `uid` and `gid`; the documents trimmed to what is true and needed.
2. **Catalog upkeep**, as releases appear: move the nixpkgs pin to the
   newest `nixpkgs-26.05-darwin` (and to 26.11's once it exists), which
   carries cached darwin builds. Where an upstream release is newer than
   any nixpkgs channel, build it here rather than wait: MySQL and MariaDB
   are already compiled from source (`images/servers.nix`), and any other
   package takes the same override of its source.
3. **Kubernetes to the newest release**, 1.37 (nixpkgs-unstable carries
   1.37.2; the pinned channel 1.36.3): the control-plane images, the
   kubelet's darwin patch rebased, `fws/kind-node` and `fws/k3s`, and a kind
   release whose node image matches.
4. **Remaining gaps**, each only where it removes a LIMITATIONS entry
   someone needs: Podman's `kube play`, a DNS server inside a stack, pod
   resource limits from the kubelet.
