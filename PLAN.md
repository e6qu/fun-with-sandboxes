# fun-with-sandboxes — plan

A Docker-API-compatible container engine for macOS that runs Compose stacks
side by side on one Mac, each isolated from the host and from the others,
built only on primitives macOS gives an unprivileged user.

Its purpose is local: a Mac runs a project's tests, Compose stacks,
Testcontainers and kind clusters in containers that start in milliseconds
and cost a process tree each, where Docker on a Mac needs a Linux VM with
its own memory, CPU and disk set aside. It is not for multi-tenant hosting
or untrusted code.

This file holds the constraints, where the project stands and what comes
next. The mechanism is in [`docs/DESIGN.md`](docs/DESIGN.md), what cannot
be done in [`LIMITATIONS.md`](LIMITATIONS.md), and how the project got
here, with the investigations behind each fix, in
[`docs/HISTORY.md`](docs/HISTORY.md).

## Constraints

- **No virtual machines.** No Hypervisor.framework, Virtualization.framework,
  or Apple's Containerization framework.
- **No root.** The engine, its install, and every container run as the
  invoking user. No `sudo`, no `launchd` daemon running as root, no kernel or
  system extensions, no changes to System Integrity Protection.
- **Go, and small C where a process start is at stake.** The engine and its
  helpers are Go and cross-compile with `CGO_ENABLED=0` for `darwin/arm64`
  from any host. What runs inside every container process start is C,
  compiled by clang on the Mac as the engine starts: the container library
  (`fwsroot.c`) and the spawn helper (`spawn.c`).
- **Performance close to native.** A container costs what its processes
  cost. An abstraction layer is taken only where its cost per call is
  nanoseconds to microseconds, and CI measures a process start
  (`BenchmarkStart`) and each interposed system call (`BenchmarkCalls`)
  against the native ones.
- **macOS's own interfaces, private ones included.** Seatbelt, its
  compiled profiles, process responsibility and `DYLD_INSERT_LIBRARIES`
  are what coding agents' sandboxes on the Mac build on too. Each has a
  test that fails if macOS changes it, such as
  `TestStartsApplyTheCompiledProfile`.
- **Real behaviour.** Every isolation property is enforced by the kernel
  through Seatbelt and is proven by a test that tries to break it. An
  option the engine cannot honour fails with an API error, never a silent
  no-op.

## Non-goals

- Intel Macs and x86_64 images. The engine runs on Apple silicon alone,
  and its images are `darwin/arm64`.
- Running Linux binaries: neither unchanged, in a VM, nor by translating
  their system calls. Images are macOS-native.
- A security boundary against hostile code. Containers run as the same user
  as the engine; Seatbelt confines them, as defence for development and
  test workloads, not multi-tenant isolation.
- Full Docker API coverage. The engine serves what Docker Compose, the
  Docker CLI, Testcontainers, Podman's clients, kind and the kubelet use
  ([`docs/DESIGN.md`](docs/DESIGN.md) §8).
- Linux platforms in builds. BuildKit builds darwin images on one worker.
- Swarm.

## Where it stands

All of this runs on a macOS CI runner with its tests:

- **Compose stacks side by side**, each with its own block of host ports and
  its own path tree, unable to reach the other's ports, paths or processes
  (`TestTwoStacksRunSideBySideInIsolation`). The Docker CLI, Docker Compose,
  buildx and BuildKit, Testcontainers with its reaper, and Podman's remote
  client and `podman-compose` drive the engine unchanged.
- **A catalog of 46 darwin images** built with Nix, configured like the
  official images ([`README.md`](README.md#images)), published to
  `ghcr.io/e6qu/fws-*` from `main`.
- **Kubernetes**: the control plane built for darwin, a patched kubelet,
  CRI served by the engine, pod networking and Services across nodes over
  TCP and UDP, and `kind create cluster --image fws/kind-node:1.35` with
  multi-node clusters, `kind load`, `kind export logs` and Testcontainers'
  k3s module (`TestKindCluster`, `TestK3sModule`).
- **Limits that keep the Mac standing**: `--memory`, `--cpus` and
  `--pids-limit` held by each container's supervisor, a QoS clamp on every
  container process, a fork ceiling under the user's process limit, and
  machine guards that pause the heaviest container under critical memory
  pressure or low disk.
- **Starts near native**: a container's process starts in about 5.6 ms
  where a native spawn takes 1.7 ms; what remains is the dynamic loader's
  cost for the inserted library (below).

## Feasibility review

Each open item, judged against the constraints, as of November 2026:

| Item | Verdict | Why |
|---|---|---|
| Image converter: Linux image in, darwin image out, by swapping ELF files for the darwin builds of the packages its package database names | **Dropped** | The files that make an image worth converting are rarely the distribution's packages: a Go or Rust service's binary, a multi-stage build's output, a vendor's tarball. None is in the package database, and each needs its source rebuilt. The images whose program is a package (PostgreSQL, Redis, nginx) are already in the catalog, versions differ between Debian and nixpkgs, and native extensions need their build toolchains and sources. What remains convertible, an interpreted app on a distribution base, is better served by building its Dockerfile, which the next item makes work. |
| Dockerfiles written for Linux (`FROM python:3.12`, `apt-get install`) | **Kept, narrowed** | The base image's mapping to a catalog image must be explicit (`-base-map`, logged in the build output), as the engine never silently substitutes an image. Package installs go through a curated table from Debian, Alpine and RPM names to nixpkgs attributes, and fail with the package's name when it has no entry. The aim is the common cases — runtimes, `curl`, `git`, build tools, client libraries — not the long tail. |
| The loader's cost for the inserted library | **Split** | Inserting any library costs the loader about 2.8 ms over a native start, since dyld does not use its prebuilt launch closures then: that is macOS's, and not pursued. `fwsroot`'s 157 interposed functions cost about 5 ms more, and the constructor nothing measurable, so interposing fewer functions, or functions fewer images bind, is in reach. |
| Kernel-enforced memory limits (`posix_spawnattr_setjetsam_ext`) | **Kept as an experiment** | XNU applies the jetsam attributes of any `posix_spawn` with no entitlement check (`bsd/kern/kern_exec.c`), so an unprivileged process may set them; whether macOS enforces a fatal limit on such a process is untested. The limit is per process, where `--memory` is the container's total, so it would back the supervisor's poll for a container's largest process, not replace it. One test on the runner decides. |
| Per-container and per-network locks | **Measure first** | Moving the slow steps out of the lock (clones, deletions, volume emptying) took the longest holds from seconds to under a second. Splitting the lock touches every engine path and risks deadlocks; it goes ahead only for the holds the `-debug` timings of `prepare`'s steps still show. |
| Port blocks that grow with a stack | **Not pursued** | A Seatbelt profile cannot change once applied, so a container started before a block was added could not reach it. |
| A server seeing clients at their member addresses | **Out of reach** | The kernel never sees member addresses; see `LIMITATIONS.md`. |
| PID and user namespaces, identity the kernel enforces, Linux images unchanged | **Out of reach** | Without root or a VM. |
| Shipping: signed and notarized releases, prebuilt C parts, an installer, a `launchd` service, state migrations, metrics | **Deferred** | Until asked for. |

## What is missing

Things the project should do and does not yet:

- **The container library's C is not fuzzed.** `fwsroot.c` is 6,400 lines
  of path mapping, socket emulation and parsing, loaded into every
  program. CI runs its probes with the library built under
  UndefinedBehaviorSanitizer, but no fuzzer drives its path mapping, and
  nothing checks that its reading of the network file matches the Go
  reader's (`internal/members`), which the Go fuzz target holds to a
  round trip.
- **Version updates are by hand.** Go modules, actions and the catalog's
  pins (nixpkgs, Playwright, Kubernetes, kind) are bumped in roadmap PRs.
  Dependabot would open a pull request per update, against the one open
  pull request this project keeps at a time.
- **`TestComposeWatch` still takes about 236 s** of its 300 s limit
  (`tests/e2e/durations.txt`); whether the snapshotter's fix ended the
  `compose up --build` stall is unconfirmed (below).

## Roadmap

Large pull requests, one open at a time, each with its tests on the macOS
runner.

1. **Faster test loop; cross-node UDP; docs** — merged as
   [#65](https://github.com/e6qu/fun-with-sandboxes/pull/65).
2. **Hardening, and the catalog refreshed** — in review. Fuzz targets for
   the Dockerfile parser, the network file and the router's header
   (`scripts/fuzz.sh`); the probes again with the C built under
   UndefinedBehaviorSanitizer; the unit tests on macOS 15 and 26;
   `staticcheck` and `govulncheck` in CI; actions on Node.js 24. A
   compiled profile keyed by the macOS build too, and the spawn helper
   compiling the text where macOS refuses the bytecode. The settings that
   were accepted and dropped are honoured (tmpfs `ro`, `noexec`, `mode`;
   `ReadonlyPaths` and `MaskedPaths`, through the profile) or refused
   (tmpfs `size`, a MAC address, a volume driver); `-v` takes propagation
   modes and `nocopy`; `docker stats` counts the supervisor's members.
   nixpkgs at the head of `nixpkgs-26.05-darwin`, the branch whose darwin
   builds are cached (Mailpit 1.33), Playwright 1.65, Go 1.26.11,
   testcontainers-go 0.45, and the module fixes govulncheck asked for.
3. **Kubernetes 1.37 and kind 0.33.** The control plane, the kubelet and
   its darwin patch moved from 1.35.0 to the release kind 0.33's node
   image ships, `fws/kind-node` to what kind 0.33 asks of a node, and the
   catalog onto nixos-26.11 once it is released, with each image's tag
   following its new version.
4. **Process starts and the engine's lock.** Cut `fwsroot`'s loader cost by
   interposing fewer functions or ones fewer images bind, guided by
   `BenchmarkCalls`' one- and four-function libraries; the jetsam
   experiment, kept as a per-process backstop if macOS enforces it and
   recorded in `LIMITATIONS.md` if not; the engine's lock split where the
   `-debug` timings still show long holds; `TestComposeWatch` run to see
   whether the build stall is gone, with its cause found if not; a fuzzer
   for the library's path mapping and a Go-against-C check of the network
   file.
5. **Dockerfiles written for Linux.** Explicit base-image mapping, the
   package-name table and `apt-get`, `apk` and `yum`/`dnf` stand-ins in
   the build that install from it, with a set of real Dockerfiles (Python,
   Node.js and Go services) built and run in the end-to-end suite.

## Open bugs

- **`docker-compose up --build` stalled for minutes** inside BuildKit's
  solve, which the snapshotter's per-layer measuring explained
  ([`docs/HISTORY.md`](docs/HISTORY.md#investigations)). The fix is in;
  `TestComposeWatch`'s recorded 236 s predates it or shows it is not the
  whole cause. Roadmap item 3 settles it.
- **The end-to-end suite takes about 10 minutes a shard**, of which loading
  images into the race-built engine is a large part, and the engine's
  time per endpoint, printed at the end of every run, is the guide to what
  to cut next.
