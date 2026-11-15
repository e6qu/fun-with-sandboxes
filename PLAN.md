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

Things the project should do and does not:

- **No fuzzing or sanitizers for the C that runs in every container
  process.** `fwsroot.c` is 6,400 lines of path mapping, socket emulation
  and parsing, loaded into every program; a memory error there corrupts
  whatever a container runs. Neither it nor the Go parsers (Dockerfile,
  the network file, the router's requests) are fuzzed, and the probes do
  not run under AddressSanitizer or UndefinedBehaviorSanitizer.
- **No fallback when the compiled profile stops applying.** The spawn helper
  fails when `sandbox_apply` refuses the bytecode, which a macOS update can
  cause; compiling the text in the process (`sandbox_init_with_parameters`
  or `sandbox_compile_string` there) would keep containers starting, at
  15 ms more each, with a warning.
- **CI on one macOS release.** Every macOS job runs on `macos-15`, while the
  engine leans on private interfaces that change with each release. The
  unit tests and probes should run on the current release too.
- **CI actions on a deprecated runtime.** `actions/checkout@v4` and
  `actions/setup-go@v5` target Node.js 20, which GitHub forces onto 24 with
  a warning; their current majors do not.
- **No static analysis beyond `go vet`, and no vulnerability scan.**
  `staticcheck` and `govulncheck` would catch what vet does not, the latter
  across BuildKit's and Kubernetes' dependencies.
- **No dependency or image version updates.** Go modules, actions and the
  catalog's pinned upstream versions are bumped by hand.
- **Settings accepted and ignored, against the rule above.** Tmpfs mount
  options (`--tmpfs /x:size=64m`, `Mount.TmpfsOptions`), a user-set MAC
  address, `VolumeDriver`, bind propagation, and `MaskedPaths` and
  `ReadonlyPaths` are taken and dropped, where a tmpfs volume's same
  options are refused. Each should be refused, or honoured where it can
  be (`ReadonlyPaths` through the profile).
- **`docker stats`' pid count disagrees with the pids limit.** Stats count
  the `ps` listing; the limit counts the supervisor's members, which
  include daemons the main process answers for.
- **`TestComposeWatch` still takes about 236 s** of its 300 s limit
  (`tests/e2e/durations.txt`); whether the snapshotter's fix ended the
  `compose up --build` stall is unconfirmed (below).

## Roadmap

Large pull requests, one open at a time, each with its tests on the macOS
runner.

1. **Faster test loop; cross-node UDP; docs** — in review as
   [#65](https://github.com/e6qu/fun-with-sandboxes/pull/65). Image
   archives kept per image, so a change to one rebuilds that one; UDP
   between pods of different nodes; a removed volume emptied off the
   engine's lock and `prepare` timed by step; the loader's cost split by
   what is interposed; the docs checked against the code and this plan.
2. **Hardening the code in every process.** Fuzz targets for the container
   library's path mapping and the Go parsers; the probes run once under
   AddressSanitizer and UndefinedBehaviorSanitizer on the macOS runner; a
   fallback to compiling the profile in the process when its bytecode does
   not apply; the ignored settings above refused or honoured, and
   `docker stats` counting the supervisor's members; `staticcheck` and `govulncheck` in CI; CI actions on current
   majors; the unit tests and probes on the current macOS release as well;
   Dependabot for Go modules and actions.
3. **Process starts and the engine's lock.** Cut `fwsroot`'s loader cost by
   interposing fewer functions or ones fewer images bind, guided by
   `BenchmarkCalls`' one- and four-function libraries; the jetsam
   experiment, kept as a per-process backstop if macOS enforces it and
   recorded in `LIMITATIONS.md` if not; the engine's lock split where the
   `-debug` timings still show long holds; `TestComposeWatch` run to see
   whether the build stall is gone, with its cause found if not.
4. **Dockerfiles written for Linux.** Explicit base-image mapping, the
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
