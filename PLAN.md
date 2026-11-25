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

Docker's CLI, Compose, buildx and BuildKit, Testcontainers for Go, Java,
Python, Node.js, Bun and Rust, Podman's remote client, `podman-compose`,
pods and `kube play`, the Dev Containers CLI and VS Code's extension,
graphical applications (`--security-opt seatbelt=gui`), `crictl`, the
kubelet, kind and k3s run against the engine in CI on macOS 15 and 26. A
catalog of 46 darwin images at 57 tags is published to
`ghcr.io/e6qu/fws-*` ([IMAGES](docs/IMAGES.md)); Kubernetes is at 1.37.2.

Seatbelt enforces a container's file reads and writes, its TCP
connections, its process listing and its IPC; its supervisor enforces its
memory, CPU, pids and disk limits, and its router and egress proxy its
network membership, bandwidth and allow lists. The container library
presents the rest: paths at their container values, ports, interfaces,
routes, users and the process tree. [LIMITATIONS](LIMITATIONS.md) lists
what is presented without being enforced.

## Roadmap

The roadmap is finished: enforced isolation
([#84](https://github.com/e6qu/fun-with-sandboxes/pull/84)), the limits
macOS can enforce ([#85](https://github.com/e6qu/fun-with-sandboxes/pull/85)),
and networks, images and the CLI
([#86](https://github.com/e6qu/fun-with-sandboxes/pull/86)). From here the
work is correctness and upkeep: the static checks CI runs stay clean, every
bug found is fixed where it is found, and compatibility with Compose,
Testcontainers, Podman, Kubernetes, kind and k3s is kept by the tests that
run them.

## Upkeep

Done at any time, never ahead of the roadmap: the catalog, Kubernetes,
etcd and kind at their newest upstream releases. How a release reaches an
image is in [IMAGES](docs/IMAGES.md#add-an-image-or-a-release).
