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

All state lives under one root, `~/.fws` by default (`fwsd -root`). The root
is resolved through symlinks at start-up, because Seatbelt matches resolved
paths.

```
~/.fws/
  docker.sock                      # the Docker Engine API
  alloc.json                       # per-stack port blocks and assignments
  images/images.json               # image index
  images/roots/<config-digest>/    # each image, flattened once
  stacks/<stack>/c/<id12>/
    r/                             # the container root: an APFS clone
    config.json  log.jsonl  profile.sb
  volumes/<name>/_data/            # named and anonymous volumes
  networks.json  volumes.json
```

An image is unpacked once, all layers applied in order with OCI whiteouts,
into `images/roots/<digest>`. On create, the engine gives the container an
**APFS clone** of it (`clonefile(2)`): copy-on-write, constant time, and near
zero bytes until the container writes. `clonefile` is the one primitive macOS
offers that fits this exactly: instant writable roots with no union mount and
no root.

The engine runs unprivileged, so layer extraction drops ownership and
setuid/setgid bits, skips device nodes (the host's `/dev` serves containers),
and refuses entries that would land outside the root.

### The "rooted at a directory, not at /" contract

We cannot remount `/`, so a container's paths live under a root the engine
names in `SANDBOX_ROOT`. An image's entrypoint maps the paths it cares about
into that root (`internal/sandboxenv.Path`): Postgres's
`PGDATA=/var/lib/postgresql/data` becomes
`$SANDBOX_ROOT/var/lib/postgresql/data`. The engine maps `PATH` itself, and
`argv[0]` is resolved inside the root.

A mount — a named volume, an anonymous volume from the image's `VOLUME`, a
bind or a tmpfs — becomes a symlink at its destination inside the root that
points at the backing host directory. The kernel follows it, and Seatbelt
checks the resolved path, so the profile grants the backing directory. A new
volume first receives the image's content at its mount point, as Docker's
copy-up does.

---

## 5. Per-stack resource assignment

A **stack** is one Compose project: the `com.docker.compose.project` label. A
container without one is a stack of its own (`run-<id>`).

1. **Ports.** Each stack holds a contiguous block of host ports
   (`-port-base`, `-port-limit`, `-block-size`; 20000–29999 in blocks of 100
   by default). Each exposed container port gets the lowest free port of the
   block the host can bind, keyed by service, replica number and container
   port, so a recreated container keeps its port.
2. **Paths.** Each stack gets `stacks/<stack>/`. No path outside it, other
   than the container's own volumes, appears in its containers' profiles.
3. **Release.** When a stack's last container goes, its block and directory
   are freed.

Assignments persist in `alloc.json`, so a restarted engine hands out the same
ports.

---

## 6. Networking without root or a network stack

The host has one loopback and one port space. The engine does not virtualise
it; it tells services their assignment through the environment.

- **Binding.** A container's own ports arrive as `SANDBOX_PORT_<n>`; the
  service listens on `127.0.0.1` at that port. Seatbelt allows binding
  exactly those ports on loopback and nothing else.
- **Discovery.** Each stack peer's ports arrive as
  `SANDBOX_<SERVICE>_PORT_<n>` with `SANDBOX_<SERVICE>_HOST=127.0.0.1`, for
  both the service name and the container name. With replicas, the names
  point at replica 1. A client of `db:5432` reads
  `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
- **Published ports.** `ports:` entries bind real host listeners in the
  engine, which forward each connection to the container's assigned port. A
  taken host port fails the start with Docker's "port is already allocated".
- **Isolation.** A container may connect to every port of its own stack's
  block and nowhere else, so stack A cannot reach stack B even though both
  live on `127.0.0.1`. That includes B's published ports.
- **Egress.** Seatbelt can allow loopback ports or every destination, nothing
  in between, so containers reach the internet through an HTTP proxy the
  engine runs for each stack on a port of the stack's block
  (`internal/egress`). Containers receive `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY`. The proxy resolves each destination once, refuses it if any
  address is loopback, link-local, multicast, unspecified or assigned to one
  of the host's interfaces, and dials an address it checked, so DNS cannot
  rebind a name onto the host. Containers with `network_mode: none`, or only
  on `internal: true` networks, get no proxy variables and their profiles omit
  the proxy's port.
- **Networks.** Compose's networks are recorded and reported faithfully, but
  they carry no traffic: reachability is the stack. A container cannot join a
  network another stack owns, and `host` and `container:` network modes are
  refused.

Discovery is by assigned port, never by a faked fixed port. Software that
hard-codes `5432` and cannot be told otherwise needs an entrypoint that
configures it.

---

## 7. Images: a curated, Nix-built catalog

Stock Docker Hub images are Linux. The catalog is built from Nixpkgs for
`aarch64-darwin` (`scripts/build-images.sh`):

- **`cmd/fws-mkimage`** copies a Nix closure into the image under
  `/nix/store`. It rewrites every absolute store reference a Mach-O file
  loads through (`LC_LOAD_DYLIB`, `LC_RPATH`) to an `@loader_path`-relative
  one with `install_name_tool`, re-signs the file ad hoc (arm64 macOS refuses
  modified code otherwise), turns absolute store symlinks into relative ones,
  and links each package's `bin/` into `/bin`. The image is then
  self-contained wherever its root lands. The profile denies the host's
  `/nix`, so the tests prove it.
- **Entrypoints are Go programs** (`images/*/entry`) that read the
  `SANDBOX_*` variables, configure the service, and `exec` it. Images carry no
  `#!` scripts: the kernel resolves interpreters on the host.
- **Catalog.** `fws/postgres:16`, with `initdb`, `POSTGRES_PASSWORD`,
  `POSTGRES_DB` and `docker-entrypoint-initdb.d/*.sql` handled the way the
  official image handles them; `fws/redis:7`; and `fws/toolbox:1`, a small
  HTTP app backed by its stack's Redis that doubles as the tests' probe.
  macOS allows only 87 System V semaphores machine-wide by default, so the
  Postgres entrypoint keeps each server's process limits small.
- **Distribution.** Archives for `docker load`, or any registry as an
  ordinary image with a `darwin/<arch>` platform:
  `scripts/publish-images.sh` pushes the archives as `<prefix>/fws-<name>`.
  CI publishes the catalog to a local registry on every pull request to prove
  the script, and the end-to-end suite pulls through the engine from an
  in-process registry. Compose files name these
  images explicitly. The engine never substitutes an image name.

---

## 8. Docker Engine API surface

Served on `unix://~/.fws/docker.sock` at API version 1.51 (minimum 1.24),
with or without the `/v1.xx` prefix:

- `GET|HEAD /_ping`, `GET /version`, `GET /info`, `GET /events`
- Containers: `create`, `json` (list, with Docker's filters), `{id}/json`,
  `start`, `stop`, `restart`, `kill`, `wait` (all three conditions),
  `rename`, `resize`, `logs` (follow, tail, since, until, timestamps),
  `attach` (hijacked, multiplexed or raw), `archive` (`HEAD`, `GET`, `PUT`:
  `docker cp`), `top`, `stats` (streamed every second, or one sample),
  `pause`, `unpause`, `update` (restart policy), `DELETE`, `prune`
- Exec: `POST /containers/{id}/exec`, `POST /exec/{id}/start` (attached,
  detached, TTY), `/exec/{id}/resize`, `GET /exec/{id}/json`
- Images: `json`, `create` (registry pull for `darwin/<arch>`), `load`,
  `{ref}/json`, `{ref}/tag`, `{ref}/history`, `get` and `{ref}/get`
  (`docker save`), `{ref}/push` (with `X-Registry-Auth`), `DELETE`
- Containers to images: `POST /commit` (with `changes`, pausing by default)
  and `GET /containers/{id}/export`
- Networks: `json`, `create`, `{id}`, `connect`, `disconnect`, `prune`, `DELETE`
- Volumes: `json`, `create`, `{name}`, `prune`, `DELETE`

Health checks run on the image's or container's own cadence (interval,
timeout, start period, retries) and report Docker's states and events. Restart
policies follow Docker's back-off.

Requests the engine cannot honour return an error rather than being
accepted: `501` for memory, CPU, PID, ulimit and device limits; privileged
mode; capabilities; non-default users; `host` networking; non-`bridge`
network drivers; custom IPAM; UDP publishing; resource updates; and
BuildKit.

`POST /grpc` and `POST /session` serve an embedded BuildKit
(`internal/buildkit`), as dockerd does, so buildx's docker driver, `docker
build` and `docker compose build` reach the real BuildKit solver and its
Dockerfile frontend. Its darwin worker supplies what is otherwise
Linux-specific:

- **Snapshotter.** A containerd snapshotter over directories: a new snapshot
  is an APFS clone of its parent, and every snapshot, views included, is
  handed out as a read-write bind mount, which BuildKit's darwin mounter uses
  without `mount(2)`.
- **Differ and applier.** containerd's walking differ and filesystem applier
  without their temporary mounts: diffs walk the two snapshot directories,
  layers unpack with the engine's own extractor.
- **Executor.** Each process runs under a Seatbelt profile confined to its
  root snapshot and mounts. BuildKit's mounts (cache, bind, secret, tmpfs)
  become symlinks at their destinations for the run and are removed after,
  so they never reach the layer. Network is the build's egress proxy, or
  none.
- **Identity mapping.** Container uid and gid 0 map to the engine's user, as
  in rootless BuildKit, so BuildKit's chowns stay ones an unprivileged
  process may make.
- **Images.** `FROM` resolves the engine's images first: an image is written
  into BuildKit's content store as a one-layer OCI image on first use. The
  `moby` exporter, which `docker build` and `--load` use, clones the result
  into the engine's image store.

The BuildKit packages the engine uses live in `third_party/buildkit`, copied
by `scripts/sync-buildkit.sh` with their imports rewritten into this module
and the patches in `third_party/buildkit-patches` applied. One patch matters:
BuildKit's darwin mounter mounts a read-only snapshot with `mount(2)`, which
needs root; the patched mounter reads the snapshot's directory directly, as it
already did for read-write ones. Keeping the packages in the module, rather
than behind a `replace` directive, keeps `go install …/cmd/fwsd@<version>`
working.

`POST /build` is the classic builder (`internal/dockerfile` parses; the
engine builds), for `DOCKER_BUILDKIT=0`. Each stage starts from an APFS clone of its base image, or an
empty root for `scratch`. `COPY` and `ADD` resolve paths inside the stage
root as a chroot would, absolute symlinks from the root, so nothing lands
outside it; they read the uploaded context, which the CLI has already filtered
through `.dockerignore`, or an earlier stage or image with `--from`. `RUN`
executes under a Seatbelt profile confined to the stage root, with internet
only through an egress proxy on a stack the build owns. The last stage's root
moves into the image store as the image, its single layer's digest computed
from a deterministic tar of the tree.

`pause` sends SIGSTOP to each of the container's process groups and `unpause`
SIGCONT; health checks and `exec` wait while a container is paused, and
stopping a paused container resumes it to deliver the stop signal.

`top` and `stats` read the host's process table through `ps`, as dockerd's
`top` does, keeping the processes of the container's process groups. CPU usage
is their accumulated CPU time; `system_cpu_usage` is the machine's CPU time
elapsed since the engine started, so the client's CPU percentage is the share
of one CPU the container used.

`docker cp` resolves paths as the container's processes do, absolute symlinks
on the host, and refuses any result outside the container's root and mounts.
It archives mounts as their contents rather than as the symlinks that
implement them.

---

## 9. Runtime launch sequence

On create: clone the image root, assign the stack's ports, realise the
mounts, and record the container. On start:

1. Render the Seatbelt profile from the container's grants (§3) and write it
   next to the container's state, outside the root.
2. Build the environment: image and container variables, `PATH` mapped into
   the root, `HOME` under the root, and the `SANDBOX_*` variables.
3. Bind the published host ports.
4. Run `/usr/bin/sandbox-exec -f <profile> <root>/<argv0> …`, the main
   process leading its own process group (or session, with a TTY). Output
   reaches the json-file log through real pipes, so the engine sees the exit
   at once even if a descendant still holds stdout.
5. When the main process exits, kill its process group, drain the output, and
   apply the restart policy.

`exec` and health checks start further processes under the same profile,
root and environment, with pipes or a pty (`creack/pty`).

---

## 10. Go implementation

Pure Go; the engine cross-compiles with `CGO_ENABLED=0` for `darwin/arm64`
and `darwin/amd64`. `clonefile` comes from `golang.org/x/sys/unix`, image
handling from `go-containerregistry`, and the API types from
`github.com/moby/moby/api`, so the JSON matches what Docker clients decode.

```
cmd/fwsd/             # the daemon
cmd/fws-mkimage/      # Nix closure -> darwin image archive
internal/alloc/       # per-stack port blocks and directories
internal/seatbelt/    # profile rendering
internal/image/       # image store, layer extraction, clonefile
internal/runtime/     # launch under sandbox-exec, process groups, pipes
internal/egress/      # the per-stack HTTP proxy to the internet
internal/procs/       # the host process table, for top and stats
internal/dockerfile/  # Dockerfile parsing and variable expansion
internal/buildkit/    # embedded BuildKit with a darwin worker
third_party/buildkit/ # the BuildKit packages it uses, patched (sync-buildkit.sh)
internal/rootfs/      # chroot-style path resolution inside a root
internal/engine/      # containers, volumes, networks, exec, health, events
internal/api/         # the Docker Engine API
internal/sandboxenv/  # the container side of the SANDBOX_* contract
images/               # entrypoints and specs for the catalog
tests/e2e/            # Docker CLI, Compose and Testcontainers end-to-end suite (own module)
```

---

## 11. Testing — the milestone's definition of done

All tests run on a **macOS CI runner** (GitHub Actions `macos-15`, Apple
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
4. **API conformance.** Drive the real `docker compose` binary and the Docker Go
   client against the daemon for the full `up`/`ps`/`logs`/`exec`/`down` cycle —
   same client code a real-Docker user runs, differing only in the socket path.

The milestone is done when (2)–(4) are green in CI on macOS. The accepted
exceptions (§1) are documented in `LIMITATIONS.md`; the tests assert that
every *action* across stacks is denied.

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
  is untested; Seatbelt profiles may not nest.

---

## 13. Milestone plan (single PR)

One branch, one PR, fully tested:

1. Repo skeleton, Go module, CI workflow (macOS runner).
2. `internal/alloc` + unit tests.
3. `internal/image` (pull/unpack + `clonefile` root) + tests.
4. `internal/runtime` (launch under `sandbox-exec`, process groups) + tests.
5. `internal/api` (the Compose subset) + Docker-client conformance tests.
6. `images/` Nix definitions for `postgres` and `redis`.
7. The two-stack concurrent integration test (§11.2) — the proof.
8. `README.md`, this `DESIGN.md`, and a `LIMITATIONS.md` kept in sync.
