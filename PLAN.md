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

1. **Graphical applications and VS Code.** Merged in
   [#67](https://github.com/e6qu/fun-with-sandboxes/pull/67) and
   [#68](https://github.com/e6qu/fun-with-sandboxes/pull/68): the Dev
   Containers CLI against the engine, VS Code's server in a dev container,
   hardened-runtime programs loading the container library, and
   `--security-opt seatbelt=gui`, with which Electron and VS Code itself,
   its extension host and integrated terminal, run inside a container.
   In #70: VS Code's Dev Containers extension on the host opens a folder
   in a dev container of the engine's, with the macOS server put where it
   looks for the Linux one it installs ([LIMITATIONS](LIMITATIONS.md)).
   Next: Chromium's own sandbox inside a container's, so that
   `--no-sandbox` goes.
2. **Starts and limits.** Merged in
   [#71](https://github.com/e6qu/fun-with-sandboxes/pull/71): per-process
   memory limits in the kernel, jetsam's, which CI's experiment showed
   macOS enforces, and in
   [#72](https://github.com/e6qu/fun-with-sandboxes/pull/72) `fwsroot`'s cost to a start measured part by
   part (`BenchmarkStartCost`): about 4 ms of interposing, some 25 µs a
   function, spread evenly over its 160, so no part of it is cheap to cut
   ([DESIGN §9](docs/DESIGN.md#9-runtime-launch-sequence)). The engine's
   lock needs no split yet: a sharded e2e run's `-debug` log shows five
   waits over 100 ms, the longest 557 ms, all behind a create.
3. **Names and pods.** Merged in
   [#72](https://github.com/e6qu/fun-with-sandboxes/pull/72): the router
   answers DNS over UDP, so that `dig` and Go's pure resolver reach the
   stack's names and the container's servers. In #73: over TCP and to
   IPv6 servers too. In #74: Network.framework's lookups, through a
   stand-in for its dnssd client, and `connectx`. Next: IPv6-only
   networks' members seen across nodes, and kind's IPv6 and dual-stack
   clusters.
4. **Catalog upkeep.** November's PostgreSQL and MariaDB once their darwin
   builds are cached; nixos-26.11; a newer Kubernetes and kind when asked
   for.
5. **Shipping**, when asked for: signed and notarized releases, prebuilt C
   parts, an installer, a `launchd` service.

## Open questions

- Whether Chromium's helpers can keep a sandbox of their own inside a
  container's: today a Chromium-based application takes `--no-sandbox`.
