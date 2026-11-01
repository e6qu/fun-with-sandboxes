# fun-with-sandboxes — Design

A no-root, no-VM container engine for macOS that speaks a subset of the Docker
Engine API, good enough to run `docker compose` stacks of macOS-native images
side by side without clashing on ports, paths, or processes, and without
reaching the host beyond what macOS strictly requires.

The engine is written in Go and cross-compiles to `darwin/arm64` and
`darwin/amd64`. It runs the workloads on the host directly — there is no virtual
machine and no privileged helper — using the only isolation primitives macOS
offers to an unprivileged process: **Seatbelt** sandbox profiles, **APFS
copy-on-write clones**, and **per-stack resource assignment**.

---

## 1. Scope and non-goals

### What it is

- A daemon that serves the Docker Engine API subset Compose v2 needs, over a
  unix socket.
- A runtime that launches each container as a host process tree confined by a
  generated Seatbelt profile, rooted in an APFS clone of the image's unpacked
  layers.
- A per-stack allocator that hands every container a private port range and a
  private data directory, so unrelated stacks never collide and never observe
  each other.

### What it is not

- **Not a Linux container runtime.** It runs macOS-native (Mach-O,
  `darwin/<arch>`) images only. Linux ELF images cannot run on macOS without a
  VM or emulation, both excluded. This is the hard boundary of the whole
  project.
- **Not root-requiring.** No step uses `sudo`, installs a kext, disables SIP,
  adds `lo0` aliases, or writes `pf` rules. If a design choice needs root, it is
  rejected and replaced with an unprivileged equivalent.
- **Not a drop-in for Docker Hub images.** Images are built for this engine (see
  §7). Stock `postgres:16` from Docker Hub is a Linux image and does not apply.

### Accepted exceptions

The brief allows small leaks where macOS gives no unprivileged alternative.
These are documented, tested for, and bounded:

- **Process listing.** `ps`/`pstree` run by the user still see container
  processes by PID and name. Seatbelt denies *acting on* them across sandboxes
  (signals, `process-info`), but not their appearance in a global listing. There
  is no unprivileged PID namespace on macOS.
- **Shared kernel surfaces.** `sysctl` values, the system clock, and the mach
  host port are global and readable. No container can write them (Seatbelt
  denies `system-*`), but they are observable.
- **System frameworks.** Every container can read `/usr/lib`, `/System/Library`,
  and the dyld shared cache. macOS binaries cannot run without them, and they
  carry no user data.

Everything else — another stack's files, ports, environment, logs, or the right
to signal its processes — is denied.

---

## 2. The core problem and the chosen resolution

On Linux, Docker isolation comes from namespaces: each container gets its own
mount table (private `/`), network stack (its own port 5432), and PID space.
macOS has **none of these** available to an unprivileged process.

Two consequences drive the entire design:

1. **No private `/`.** A container process sees the host's real paths. We cannot
   mount an overlay at `/` without root. → We give each container an APFS clone
   as its root *directory* and make the image expect to live there (§4), rather
   than pretending it is at `/`.

2. **No private network stack.** Every container shares the host's single
   loopback and port space. Two containers cannot both own `:5432`. → We do not
   fake per-container ports. Instead the engine **assigns** each container real,
   distinct host ports and real data paths, and passes them to the service
   through configuration — the mechanism the user has accepted (§5, §6).

This is the pivot of the project: **isolation by assignment, enforced by
Seatbelt**, not isolation by virtualization. The engine is the authority that
hands out non-overlapping resources; Seatbelt is the fence that stops a
container using anything it was not handed.

---

## 3. Isolation model — what each primitive provides

| Dimension | Primitive | Guarantee | Gap (accepted) |
|---|---|---|---|
| Filesystem write | Seatbelt `(deny file-write*)` + allow only the container root and its volumes | A container writes only inside its own clone and mounts | — |
| Filesystem read of host | Seatbelt `(deny file-read*)` + allow system frameworks and the container root | No read of `$HOME`, other stacks' roots, or user data | System libs/frameworks readable (needed) |
| Cross-stack visibility | Per-stack directory tree + per-stack Seatbelt path allowances | Stack A's profile names only A's paths; A cannot read or write B | — |
| Ports | Engine allocates a disjoint range per stack; Seatbelt `(deny network-bind)` except the assigned ports | Two stacks never clash; a container binds only its grants | Host sees all listening sockets globally |
| Process signalling | Seatbelt `(deny signal)` except same-sandbox; `(deny process-info*)` except self | A cannot `kill` or inspect B's processes | `ps` lists them (no PID namespace) |
| Resource limits | `setrlimit` (fds, procs, core); QoS class | Bounded fds/procs | No hard CPU/memory cap (needs root/VM) → `mem_limit`/`cpus` are **rejected**, not ignored |
| Identity | Runs as the invoking user | — | No `user:`-switch to arbitrary UID (needs root) → non-default `user:` is **rejected** |

Rejections are deliberate: an unsupported knob returns a clear API error rather
than silently doing nothing. Silent no-ops would be a fidelity bug.

---

## 4. Image and root filesystem

### Layout

An image is unpacked once into a content-addressed store:

```
~/Library/Caches/fun-with-sandboxes/
  images/<digest>/rootfs/        # unpacked, read-only reference copy
  layers/<digest>/               # per-layer cache
```

On container create, the engine makes an **APFS clone** of the image rootfs
(`clonefile(2)`, copy-on-write, O(1) and near-zero bytes until written):

```
~/Library/Application Support/fun-with-sandboxes/
  stacks/<stack>/containers/<cid>/rootfs/   # the clone, writable
  stacks/<stack>/volumes/<vol>/             # named/anonymous volumes
```

`clonefile` is the one genuinely good fit macOS offers: instant per-container
writable roots with no union-mount machinery and no root.

### The "rooted at a directory, not at /" contract

Because we cannot remount `/`, images are built (§7) so that every path they
care about is **relative to a root the engine supplies in an environment
variable** (call it `$SANDBOX_ROOT`), or is a system path under `/usr`,
`/System` that is identical on every host. Concretely:

- The service's data dir, config dir, sockets, and pid files live under
  `$SANDBOX_ROOT`, e.g. Postgres `PGDATA=$SANDBOX_ROOT/var/lib/postgresql/data`.
- The service's own binaries and libraries are shipped in the image under
  `$SANDBOX_ROOT/usr` and loaded from there.
- Only the immutable macOS system libraries are read from their real host
  locations.

This is the concrete form of "services take their data paths from
configuration." Seatbelt then allows writes only under that one root.

---

## 5. Per-stack resource assignment

A **stack** is one `docker compose` project (its project name). The allocator
owns three namespaces, each partitioned per stack so two stacks are disjoint by
construction:

1. **Ports.** Each stack is assigned a contiguous block of free host ports
   (probed with `net.Listen` then released). Every `ports:` and inter-service
   port a container needs is mapped into that block. The mapping table is the
   stack's source of truth.

2. **Paths.** Each stack gets `stacks/<stack>/…` (above). No path outside it
   appears in any of its containers' Seatbelt profiles.

3. **Names/DNS.** Each stack gets a view that maps service names to the assigned
   host ports (§6). Names are not globally unique across stacks; the view is
   per-stack, so stack A's `db` and stack B's `db` never resolve to each other.

The allocator persists assignments so `compose up` → `compose down` → `up` is
stable, and releases them on `down`/prune.

---

## 6. Networking without root or a network stack

The host has one loopback and one port space. We do not virtualize it; we make
the assignment visible to services the way the user accepted — through config.

- **Binding.** A service reads the port it should listen on from its
  environment/config (e.g. `PGPORT`, `--port`, a rendered config file). The
  engine sets that to the stack's assigned port. Seatbelt allows binding exactly
  that port and denies the rest.

- **Service discovery.** The engine writes, per stack, the mapping from service
  name to `127.0.0.1:<assigned-port>` into:
  - environment variables the Compose service template references
    (`${DB_HOST}`, `${DB_PORT}`), and
  - an `/etc/hosts`-style file and a small resolver config placed under the
    container root,
  so `db:5432` in a compose file is expressed as
  `${DB_HOST}:${DB_PORT}` → `127.0.0.1:<assigned>`.

- **Published ports (`-p`).** The container's assigned port *is* a real host
  port, so "publishing" is mostly identity; where a compose file requests a
  specific host port, the engine records it in the stack block (and errors on a
  cross-stack clash rather than silently remapping).

- **Isolation.** A container's Seatbelt profile denies `network-bind` and
  `network-outbound` except to its own assigned ports and its stack's ports.
  Stack A cannot connect to stack B's services even though both are on
  `127.0.0.1`, because A's profile never names B's ports.

The honest limitation: discovery is by assigned port, not a faked fixed port.
Services that hard-code `5432` with no way to configure the port do not work;
services that read the port from config (the accepted contract) do.

---

## 7. Images: a curated, Nix-built catalog

Stock Docker Hub images are Linux. We build a small catalog of macOS-native
images instead:

- **Build tool.** Nixpkgs `aarch64-darwin`/`x86_64-darwin` already builds
  Postgres, Redis, nginx, etc. A `nix build` closure is converted to OCI layers
  with a `darwin/<arch>` platform entry and an entrypoint that reads
  `$SANDBOX_ROOT`, the assigned port, and the data path from the environment.
- **Shell and coreutils.** Images ship their own `sh`/coreutils under
  `$SANDBOX_ROOT/usr` (SIP-protected system binaries strip custom env, so the
  image must not depend on `/bin/sh` for its entrypoint; see §9).
- **Catalog (initial).** `postgres`, `redis`. Enough to prove a real two-service
  stack. More added the same way.
- **Distribution.** Pushed to a registry as a normal OCI index; compose files
  name them explicitly (e.g. `image: ghcr.io/e6qu/fws-postgres:16`). The engine
  never silently substitutes an image name — that would be consumer-aware
  special-casing.

---

## 8. Docker Engine API surface

Served over `unix://~/Library/.../fun-with-sandboxes.sock`. Compose v2 drives
these; we implement exactly the subset it calls:

- `GET /_ping`, `GET /version`, `GET /info`
- Images: `GET /images/json`, `POST /images/create` (pull), `GET
  /images/{name}/json`, `POST /images/{name}/tag`, `DELETE /images/{name}`
- Containers: `POST /containers/create`, `/start`, `/stop`, `/kill`, `/wait`,
  `/restart`, `GET /containers/json`, `GET /containers/{id}/json`, `DELETE`,
  `GET /containers/{id}/logs`, `POST /containers/{id}/attach`, `GET
  /containers/{id}/stats`
- Exec: `POST /containers/{id}/exec`, `POST /exec/{id}/start`, `GET
  /exec/{id}/json` (healthchecks, `compose exec`)
- Networks: `POST /networks/create`, `GET /networks`, `DELETE`, `/connect`,
  `/disconnect` (modeled as the per-stack discovery view, §6)
- Volumes: `POST /volumes/create`, `GET /volumes`, `DELETE`
- `GET /events` (compose waits on these)

Unsupported `HostConfig` knobs (`Memory`, `NanoCpus`, non-default `User`,
privileged, cap-add, arbitrary bind of host paths outside the stack) return a
`400` with a precise message. No silent acceptance.

---

## 9. Runtime launch sequence

Per container start:

1. Clone the image rootfs → container root (`clonefile`).
2. Allocate/look up the stack's ports and paths; render the service's config and
   environment (`$SANDBOX_ROOT`, assigned ports, discovery map).
3. Write the container's `/etc/hosts`-style discovery file and volume mounts
   (volumes are directories under the stack tree, referenced by path — no bind
   mount needed).
4. Generate the Seatbelt profile from the stack's grants (allowed read paths,
   the single writable root + volumes, the allowed ports, same-sandbox signals).
5. Launch the entrypoint under the profile. Because `DYLD_*` and custom env are
   stripped when the kernel execs a SIP-protected binary, the engine execs the
   **image's own** entrypoint binary (shipped in the image, not a system one),
   which then keeps the environment and starts the service.
6. Track the process tree via kqueue `EVFILT_PROC` (`NOTE_FORK`/`NOTE_EXIT`) for
   `wait`, exit codes, restart policy, and stats (`proc_pid_rusage`).

`exec`/attach spawn additional processes into the **same** profile, root, and
env, with pipes or a pty (`creack/pty`).

---

## 10. Go implementation

Pure Go where possible; the macOS-specific calls use direct syscalls or
`purego`/`cgo` for the handful without a `syscall` wrapper:

- `clonefile(2)` — `golang.org/x/sys/unix` (`unix.Clonefile`) where available,
  else `syscall.Syscall` on the `clonefileat` number.
- `sandbox_init`/`sandbox-exec` — exec `/usr/bin/sandbox-exec -p <profile>` for
  v1 (simplest, no private API); the profile is generated per container.
- kqueue — `unix.Kqueue`/`unix.Kevent`.
- `proc_pid_rusage`, libproc listing — via `purego` dlopen of libproc, or cgo.

Module layout:

```
cmd/fwsd/            # the daemon (Docker API server)
cmd/fws/             # a thin CLI for smoke tests
internal/api/        # Docker Engine API handlers
internal/runtime/    # launch, Seatbelt profile gen, kqueue tracking
internal/image/      # OCI pull/unpack, clonefile root
internal/alloc/      # per-stack port/path/name allocator
internal/compose/    # fixtures + the discovery-view writer
images/              # Nix definitions for the catalog
```

Cross-compiles with `GOOS=darwin GOARCH=arm64`/`amd64`. CGO parts build on the
macOS CI runner; the `purego` path keeps most of it CGO-free.

---

## 11. Testing — the milestone's definition of done

All tests run on a **macOS CI runner** (GitHub Actions `macos-14`, Apple
Silicon). No `t.Skip` for "tool missing" — required tools are installed in the
workflow.

1. **Unit.** Allocator disjointness (no two stacks share a port/path), Seatbelt
   profile generation, API request/response shapes.
2. **Runtime integration (the core proof).** Build the Nix `postgres` and
   `redis` images, then bring up **two independent compose stacks
   simultaneously**, each a `postgres` + `redis` + a tiny app:
   - Assert both stacks are healthy and serve queries concurrently.
   - Assert their assigned ports differ and neither can reach the other's ports
     (attempt a cross-stack connect → refused by Seatbelt).
   - Assert stack A cannot read or write any path under stack B (attempt →
     denied).
   - Assert `compose down` on A leaves B fully working, and frees A's ports.
3. **Isolation assertions.** From inside a container: writing outside its root
   fails; reading `$HOME` fails; signalling the other stack's PID fails; reading
   the other stack's data dir fails.
4. **Accepted-exception assertions.** Document-and-verify that `ps` *can* list
   the other stack (so the exception is explicit and bounded), while every
   *action* on it is denied.
5. **API conformance.** Drive the real `docker compose` binary and the Docker Go
   client against the daemon for the full `up`/`ps`/`logs`/`exec`/`down` cycle —
   same client code a real-Docker user runs, differing only in the socket path.

The milestone is done when (2)–(5) are green in CI on macOS.

---

## 12. Known limitations (stated, not hidden)

- macOS-native images only; no Linux images, ever, without a VM.
- Images must be built for the `$SANDBOX_ROOT` + configurable-port contract;
  stock images don't run.
- No hard CPU/memory limits; those knobs are rejected, not emulated.
- No `user:` switch to arbitrary UIDs; runs as the invoking user.
- Process listing (`ps`/`pstree`) crosses sandboxes; every action does not.
- `sandbox-exec` is Apple-deprecated (since 2016) but still functional and used
  by Chrome, Bazel, and the major coding-agent sandboxes; there is no supported
  replacement for this use.
- Running the engine *inside* another Seatbelt sandbox (e.g. a coding agent's)
  may fail to nest; verified early in CI.

---

## 13. Milestone plan (single PR)

One branch, one PR, fully tested:

1. Repo skeleton, Go module, CI workflow (macOS runner).
2. `internal/alloc` + unit tests.
3. `internal/image` (pull/unpack + `clonefile` root) + tests.
4. `internal/runtime` (Seatbelt profile gen, launch, kqueue) + tests.
5. `internal/api` (the Compose subset) + Docker-client conformance tests.
6. `images/` Nix definitions for `postgres` and `redis`.
7. The two-stack concurrent integration test (§11.2) — the proof.
8. `README.md`, this `DESIGN.md`, and a `LIMITATIONS.md` kept in sync.
