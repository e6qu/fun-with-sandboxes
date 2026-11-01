# fun-with-sandboxes — plan

A Docker-API-compatible container engine for macOS that runs Compose stacks
side by side on one Mac, each isolated from the host and from the others,
built only on primitives macOS gives an unprivileged user.

Detailed design: [`docs/DESIGN.md`](docs/DESIGN.md). This file is the
top-level plan and milestone; the design doc is the full mechanism.

## Constraints

- **No virtual machines.** No Hypervisor.framework, Virtualization.framework,
  or Apple's Containerization framework.
- **No root.** The engine, its install, and every container run as the
  invoking user. No `sudo`, no `launchd` daemon running as root, no kernel or
  system extensions, no changes to System Integrity Protection.
- **Go.** Every component is Go and cross-compiles (`GOOS=darwin`,
  `GOARCH=arm64|amd64`) from any host; the macOS-only syscalls use
  `golang.org/x/sys/unix` or `purego`, keeping most of the tree CGO-free.
- **Real behaviour.** Every isolation property claimed below is enforced by
  the kernel through Seatbelt and is proven by a test that tries to break it.

## Non-goals

- Running Linux images. Without a VM nothing on macOS executes Linux ELF
  binaries, so images are macOS-native (`darwin/arm64`, `darwin/amd64`).
- A security boundary against hostile code. Containers run as the same user as
  the engine; Seatbelt confines them, but it is defence for development and
  test workloads, not multi-tenant isolation.
- Full Docker API coverage. The engine serves the subset Docker Compose, the
  Docker CLI, and Testcontainers use (listed in the design doc).
- `docker build`. Images come from the Nix image pipeline and reach the engine
  through `docker load` or a registry pull.

## Core idea

macOS gives an unprivileged process no private `/` and no private network
stack, so isolation is **by assignment, enforced by Seatbelt** — not by
virtualization. The engine is the authority that hands each stack a disjoint
port range and path tree; Seatbelt is the fence that stops a container using
anything it was not handed. Services take their ports and data paths from
configuration, which is how per-stack isolation is expressed without a private
mount or network namespace.

## Isolation model

| Resource | Mechanism | Guarantee | Accepted exception |
|---|---|---|---|
| Filesystem writes | APFS `clonefile` root per container; Seatbelt `file-write*` allowed only on the container root, its volumes, and `/dev/null`-style devices | A container writes nowhere else | — |
| Host reads | Seatbelt `file-read*` denied on `$HOME`, `/Users`, `/Volumes`, `/private/var/folders`, and other stacks' state; allowed on the container root, its volumes, and the system paths every macOS process needs (`/System`, `/usr/lib`, `/usr/share/locale`, `/usr/share/zoneinfo`, `/private/var/db/timezone`, `/dev`) | A container reads its own files and the OS, nothing of the user's | The macOS system tree is readable |
| Cross-stack visibility | Per-stack path tree; a stack's Seatbelt profile names only its own paths and ports | Stack A cannot read, write, or connect to stack B | — |
| Processes | Seatbelt `signal` and `process-info*` restricted to the same sandbox | A container cannot signal or inspect another stack's processes | `ps`/`pstree` still list them (no unprivileged PID namespace) |
| Ports | Engine allocates a disjoint host-port block per stack; Seatbelt `network-bind`/`network-outbound` allowed only on the stack's ports | Two stacks never clash; a container binds only its grants | Host sees all listening sockets globally |
| Resource limits | `setrlimit` (fds, procs), QoS class | Bounded fds/procs | No hard CPU/memory cap → `mem_limit`/`cpus` are rejected, not emulated |
| Identity | Runs as the invoking user | — | No `user:` switch to arbitrary UID (needs root) → rejected |

Rejected knobs return a precise API error; silent no-ops would be a fidelity
bug.

## Components (Go)

```
cmd/fwsd/            # the daemon: Docker Engine API over a unix socket
cmd/fws/             # a thin CLI for smoke tests
internal/api/        # Docker Engine API handlers (the Compose subset)
internal/runtime/    # launch, Seatbelt profile generation, kqueue tracking
internal/image/      # OCI pull/unpack, clonefile root
internal/alloc/      # per-stack port/path/name allocator
internal/compose/    # fixtures + the per-stack discovery-view writer
images/              # Nix definitions for the darwin image catalog
```

## Image catalog

A small Nix-built catalog for `aarch64-darwin`/`x86_64-darwin`, converted to
OCI layers with a `darwin/<arch>` platform entry and an entrypoint that reads
`$SANDBOX_ROOT`, its assigned port, and its data path from the environment.
Initial catalog: `postgres`, `redis`. Compose files name these images
explicitly; the engine never silently substitutes an image name.

## Milestone — definition of done

One branch, one PR, fully tested on a macOS CI runner (`macos-14`, Apple
Silicon), no `t.Skip` for missing tools:

1. Repo skeleton, Go module, macOS CI workflow.
2. `internal/alloc` + unit tests (per-stack disjointness).
3. `internal/image` (pull/unpack + `clonefile` root) + tests.
4. `internal/runtime` (Seatbelt profile gen, launch, kqueue) + tests.
5. `internal/api` (Compose subset) + Docker-client conformance tests.
6. `images/` Nix definitions for `postgres` and `redis`.
7. **The proof:** two independent Compose stacks (`postgres` + `redis` + a tiny
   app each) brought up concurrently, asserting they
   - serve queries at the same time on different assigned ports,
   - cannot connect to each other's ports (Seatbelt-refused),
   - cannot read or write each other's paths,
   - and that `down` on one leaves the other working and frees its ports.
8. `README.md`, `docs/DESIGN.md`, and a `LIMITATIONS.md` kept in sync.
