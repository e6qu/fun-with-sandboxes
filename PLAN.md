# fun-with-sandboxes — plan

A container engine for macOS that serves the Docker Engine API and Podman's
libpod API, so the stock `docker` CLI, Docker Compose, `podman`,
Testcontainers and kind drive it unchanged. Its containers run real macOS
binaries and tooling, each confined by Seatbelt, with the Docker primitives
that translate to macOS and a clear refusal of those that do not.

Mechanism: [`docs/DESIGN.md`](docs/DESIGN.md). What cannot be done:
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
  store: a blob and an unpacked layer are kept once and shared by every
  image that holds them, manifests and digests survive a pull, save, load
  and push byte for byte, and the engine speaks the distribution protocol
  to any registry.
- **Close to native.** A container costs what its processes cost; CI
  measures a process start and each interposed call against the native one.
- **Go, and C only inside a process start**, cross-compiled for
  `darwin/arm64` alone.

## Non-goals

- Linux binaries, in any form: unchanged images, a VM, system-call
  translation, a converter from Linux images, or Dockerfiles that install
  with `apt-get`, `apk` or `yum`. Images are built `FROM` darwin images.
- A security boundary against hostile code, Swarm, Intel Macs.

## Where it stands

Docker's CLI, Compose, buildx and BuildKit, Testcontainers, Podman's remote
client and `podman-compose`, `crictl`, the kubelet and kind run against the
engine in CI on macOS 15 and 26, with a catalog of 46 darwin images
published to `ghcr.io/e6qu/fws-*`. Kubernetes is held at 1.35.0, the node
image of kind 0.31, until a newer one is asked for.

## Roadmap

One pull request open at a time.

1. **In review,
   [#66](https://github.com/e6qu/fun-with-sandboxes/pull/66).**
   - Checks: staticcheck, govulncheck, fuzzing, the C under
     UndefinedBehaviorSanitizer, macOS 26.
   - Settings once accepted and dropped are honoured or refused; compiled
     profiles survive a macOS update.
   - `compose up --build` no longer stalls on a base image's layers.
   - Podman pods with infra containers; `--dns`, `--dns-search` and
     `--dns-option`; networks with several subnets a family and IPv6-only
     networks; `docker update --blkio-weight` on a running container; UDP
     ports published from a nested engine.
   - The image store against the OCI specifications: digests and diff IDs
     verified, times, owners and modes kept through layers and diffs,
     unpacked layers shared between images.
   - The catalog on `nixpkgs-26.05-darwin`, Playwright 1.65.
2. **Devcontainers.** The Dev Containers CLI and VS Code's Dev Containers
   extension against the engine: a `devcontainer.json` on a darwin image,
   its features and lifecycle commands, the workspace mount, port
   forwarding, and VS Code's server running inside the container. Then a
   whole graphical Electron application, VS Code itself, running in a
   container: which needs the window server, pasteboard and GPU services a
   container's profile denies today, granted per container.
3. **Starts and limits.** `fwsroot`'s cost to the loader cut by interposing
   fewer functions; per-process memory limits in the kernel if CI's jetsam
   experiment shows macOS enforces them; the engine's lock split where
   `-debug` timings still show long holds; a fuzzer for the library's path
   mapping.
4. **Catalog upkeep.** November's PostgreSQL and MariaDB once their darwin
   builds are cached; nixos-26.11; a newer Kubernetes and kind when asked
   for.
5. **Shipping**, when asked for: signed and notarized releases, prebuilt C
   parts, an installer, a `launchd` service.

## Open questions

- Whether an Electron application can run under a Seatbelt profile that
  still confines it: it needs WindowServer and other Mach services, and
  Chromium sandboxes its own helpers, which a process already in a sandbox
  cannot do the same way. Item 2 starts by finding out.
- Whether macOS enforces a jetsam limit an unprivileged parent sets: CI's
  `jetsam experiment` step prints the answer.
