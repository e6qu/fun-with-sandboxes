# Plan

fws is a container engine for macOS that serves the Docker Engine API and
Podman's libpod API, so the stock `docker` CLI, Docker Compose, `podman`,
Testcontainers and kind drive it unchanged. Its containers run macOS
binaries, each confined by Seatbelt. It gives the Docker primitives that
translate to macOS and refuses those that do not.

How its parts fit together is in [ARCHITECTURE](docs/ARCHITECTURE.md),
how each works in [DESIGN](docs/DESIGN.md), what differs from Docker in
[LIMITATIONS](LIMITATIONS.md), and how it got here in
[HISTORY](docs/HISTORY.md).

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in [RFC 2119](https://www.rfc-editor.org/info/rfc2119).

## Constraints

- **No virtual machine, no root.** Everything MUST run as the invoking
  user: the engine MUST NOT need a hypervisor, `sudo`, a root daemon, a
  kernel extension or a change to System Integrity Protection.
- **Isolation the kernel enforces.** Seatbelt MUST confine every container
  process, and a test MUST try to break each property claimed.
- **Honour or refuse.** An option the engine cannot enforce MUST fail with
  a Docker API error; it MUST NOT be accepted and dropped.
- **OCI throughout.** Images MUST be layered OCI images in a
  content-addressed store; manifests and digests MUST survive a pull, save,
  load and push byte for byte, and the engine MUST speak the distribution
  protocol to any registry.
- **Close to native.** A container SHOULD cost what its processes cost; CI
  measures a process start and each interposed call against the native one.
- **Go, and C only inside a process start.** The engine MUST be written in
  Go, with C only inside a process start, and built for `darwin/arm64`
  alone.

## Scope

Dockerfiles built `FROM` fws's base images are in scope: fws provides the
darwin base images (`fws/base`, the language and service images), and
`docker build`, buildx and Compose build on them whatever macOS can run.

## Non-goals

- Linux binaries, in any form: unchanged images, a VM, system-call
  translation, a converter from Linux images, or Linux-specific Dockerfiles
  (`apt-get`, `apk`, `yum`) made to build.
- A security boundary against hostile code, Swarm, Intel Macs.
- Packaging for distribution: signed or notarized releases, installers, a
  `launchd` service. The engine is installed with `go install`.

## Status

The roadmap is finished. Its last three steps were enforced isolation
([#84](https://github.com/e6qu/fun-with-sandboxes/pull/84)), the limits
macOS can enforce
([#85](https://github.com/e6qu/fun-with-sandboxes/pull/85)), and networks,
images and the CLI
([#86](https://github.com/e6qu/fun-with-sandboxes/pull/86)).

CI's end-to-end tests run every client [README](README.md#what-drives-it)
lists against the engine on macOS 15; the engine's own tests run on macOS
15 and 26. The catalog holds 46 darwin images at 57 tags, published to
`ghcr.io/e6qu/fws-*` ([IMAGES](docs/IMAGES.md)); Kubernetes is at 1.37.2.

Seatbelt enforces a container's file reads and writes, its TCP
connections, its process listing and its POSIX IPC. Its supervisor and
the kernel's per-process limits enforce its memory, CPU and pids limits, a
disk image of its own its disk quota, and its router and egress proxy its
network membership, bandwidth and allow lists. The container library
presents the rest: paths, ports, interfaces, routes, users and the process
tree. [LIMITATIONS](LIMITATIONS.md) lists what is presented without being
enforced.

The work from here is correctness and upkeep. The static checks CI runs
MUST stay clean, every bug found MUST be fixed where it is found, and the
tests that run Compose, Testcontainers, Podman, Kubernetes, kind and k3s
keep compatibility with them.

## Upkeep

The catalog, Kubernetes, etcd and kind SHOULD be at their newest upstream
releases. This upkeep MAY be done at any time, but MUST NOT go ahead of
the roadmap. An update takes the newest release; where nixpkgs lags, the
release is built here
([IMAGE-AUTHORING](docs/IMAGE-AUTHORING.md#update-an-image-to-a-new-upstream-release)).
