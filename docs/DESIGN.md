# fun-with-sandboxes — Design

A no-root, no-VM container engine for macOS that speaks a subset of the Docker
Engine API, enough to run `docker compose` stacks of macOS-native images side
by side without clashing on ports, paths or processes, and without reaching
the host beyond what macOS requires.

The engine is written in Go for `darwin/arm64`, Apple silicon alone. It runs
the workloads on the host, with no virtual machine and no privileged helper,
using the only isolation primitives macOS offers an unprivileged process:
**Seatbelt** sandbox profiles, **APFS copy-on-write clones**, and **per-stack
resource assignment**.

---

## 1. Scope and non-goals

### What it is

- A daemon that serves the Docker Engine API subset Compose v2 needs, over a
  unix socket.
- A runtime that launches each container as a host process tree confined by a
  generated Seatbelt profile, rooted in an APFS clone of the image's unpacked
  layers.
- A per-stack allocator that hands every stack a private port range and a
  private directory, so unrelated stacks never collide or observe each
  other.

### What it is not

- **Not a multi-tenant or untrusted-code sandbox.** It is a local runtime for
  a developer's tests and services; Seatbelt keeps stacks apart and the
  user's data out of reach, against mistakes rather than attackers.
- **Not a Linux container runtime.** It runs macOS-native (Mach-O,
  `darwin/<arch>`) images only; Linux ELF images need a VM or emulation, both
  excluded. Images come from the Nix-built catalog (§7) or from Dockerfiles
  built on it; Dockerfiles written for Linux are a non-goal
  ([`PLAN.md`](../PLAN.md)).
- **Not root-requiring.** No step uses `sudo`, installs a kext, disables SIP,
  adds `lo0` aliases, or writes `pf` rules. A design choice that needs root is
  replaced with an unprivileged equivalent.
- **Not a drop-in for Docker Hub images.** Stock `postgres:16` is a Linux
  image and does not apply (§7).

### Accepted exceptions

Where macOS gives an unprivileged process no alternative, a container sees
more of the host than under Docker, though it can act on none of it:

- **Host pids.** Processes keep their host pids. Seatbelt denies the
  process table's listings (`seatbelt.ProcListings`), but a program that
  loads no container library and knows another process's pid still reads
  its `kern.proc.pid` entry. Seatbelt denies signalling or inspecting
  another sandbox's processes.
- **Shared kernel surfaces.** `sysctl` values, the system clock and the
  Mach host port are global and readable; Seatbelt denies writing them.
- **The host's system tree.** A container reads, as under chroot, its own
  root, volumes and files, the system tree every macOS program loads
  (`seatbelt.SystemReadPaths`: `/System`, `/usr/lib`, `/usr/share`, the
  dyld and time zone databases, `/Library/Apple`, `/dev`) and the few host
  files a root of its own would hold (`seatbelt.HostReadPaths`: the shells,
  the time zone, the CA certificates, the services and protocols
  databases), and nothing else of the host. A container granted the user's
  session reads the host but its data roots (`seatbelt.DeniedReadRoots`)
  and the engine's state, as graphical applications read fonts,
  preferences and bundles all over it.

Another stack's files, ports, environment and logs, and the right to signal
its processes, stay denied. [`LIMITATIONS.md`](../LIMITATIONS.md) lists every
gap.

---

## 2. The core problem and the chosen resolution

On Linux, Docker isolation comes from namespaces: each container gets its own
mount table, network stack and PID space. macOS offers none of these to an
unprivileged process. Two consequences drive the design:

1. **No private `/`.** A container process sees the host's real paths, and
   an overlay cannot be mounted at `/` without root. Each container gets an
   APFS clone as its root *directory*, and the container library presents
   it at `/` (§3a, §4).
2. **No private network stack.** Every container shares the host's loopback
   and port space; two containers cannot both own `:5432`. The engine
   **assigns** each container real, distinct host ports, and the container
   library binds a container port's host port in its place, so a service
   listens on its container port as written (§5, §6).

This is **isolation by assignment, enforced by Seatbelt**: the engine hands
out non-overlapping resources, and Seatbelt stops a container using anything
it was not handed.

---

## 3. Isolation model — what each primitive provides

| Dimension | Primitive | Guarantee | Gap (accepted) |
|---|---|---|---|
| Filesystem write | Seatbelt `(deny file-write*)`, then allow the container root and its volumes | A container writes only inside its own clone and mounts | — |
| Filesystem read of host | Seatbelt `(deny file-read*)`, then allow the root, its volumes, the container library, its network file, the system tree and the host files a root would hold; a container granted the user's session reads the host but its data roots and the engine's state | No read of the host outside the root, volumes and system tree: not `$HOME`, other stacks' roots, `/private/etc`, `/usr/bin` or shared temporary directories | The system tree is readable |
| Cross-stack visibility | Per-stack directory tree and per-stack Seatbelt path allowances | Stack A's profile names only A's paths; A cannot read or write B | — |
| Ports | Engine allocates a disjoint range per stack; Seatbelt `(deny network-bind)` and `(deny network-outbound)` except the container's own ports, its router and its egress proxy | Two stacks never clash; a container binds only its grants and reaches other members only through its router | Host sees all listening sockets globally |
| Process signalling | Seatbelt `(deny signal)` and `(deny process-info*)` except same-sandbox, and `(deny sysctl-read)` of the process table's listings; the library lists the sandbox's processes from its shim (`pidns.OpTable`). `--pid host` allows both (`FWS_PIDNS=host`); `--pid container:` shares a namespace through the shims (§3a) | A cannot `kill`, inspect or list B's processes | pids are the host's |
| Default deny | Seatbelt `(deny default)` on Apple's `system.sb` baseline, each operation class re-allowed by name | No IOKit, Mach registration, preferences, AppleEvents, kernel control sockets or anything else the profile does not name | — |
| IPC | Seatbelt `ipc-posix-name-prefix` filters; the library's per-container System V keys | POSIX shared memory and semaphores confined to a per-container name prefix; System V keys hashed with it; `--ipc none` denies all IPC | A System V key is presented, not confined: a guessed hashed key reaches another container's object |
| Resource limits | Per-container supervisor (§3a); `setrlimit`, the nice value, the disk I/O tier and a QoS clamp through the spawn helper | Memory, CPU, pids caps, `--ulimit`, priority and disk I/O tiers | Enforced by ending, stopping or killing processes, not by refusing allocations; the library refuses forks at the pids limit |
| Identity | Runs as the invoking user; the library presents the user `--user` or `USER` names, resolved against the container's own `/etc/passwd` and `/etc/group` | Programs see the container's user and groups, as under Docker | Presented, not enforced |

An unsupported knob returns a clear API error rather than silently doing
nothing. [`SEATBELT.md`](SEATBELT.md) shows the profile's sections and how to
read a container's profile and its denials.

### 3a. Isolation and limits without cgroups

macOS gives an unprivileged process no cgroups, namespaces or capability
sets. The engine composes what it does give — `posix_spawn` attributes,
`setrlimit`, `setpriority`, `setiopolicy_np`, `proc_pid_rusage`, kqueue
process events, `SIGSTOP`/`SIGCONT` and Seatbelt's filters — into the same
knobs, each enforced by a real mechanism or refused.

#### Starting a process

- **Spawn helper.** Every container's main process and every build step
  starts as the spawn helper (`internal/pathmap/src/spawn.c`), through
  `posix_spawn` with responsibility disclaimed, so it inherits no privacy
  (TCC) grant of whatever launched the engine. The helper applies
  `--ulimit` (`nofile`, `fsize`, `cpu`, `core`, `stack`), the nice value
  `--cpu-shares` maps to, the disk I/O tier `--blkio-weight` maps to, the
  controlling terminal and the fork ceiling; applies the compiled Seatbelt
  profile (`sandbox_apply`); and executes the command with the container
  library loaded, through `posix_spawn` with `POSIX_SPAWN_SETEXEC`, the one
  exec that takes a QoS clamp. A limit the user cannot set fails the start,
  reported on a close-on-exec status pipe.
- **Compiled profiles.** Compiling a profile's text takes libsandbox about
  15 ms; applying bytecode takes the kernel about 35 µs. While the shim
  starts, the engine renders `profile.sb` (`internal/seatbelt`) and compiles
  it with `sandbox_compile_string` (`libsystem.SandboxCompile`) to
  `profile.bin`, keyed by the SHA-256 of the text and `kern.osversion`, so
  an unchanged text compiles nothing. Where macOS refuses the bytecode, the
  helper compiles the text through the deprecated `sandbox_init` instead.
- **Fork ceiling.** The helper lowers the soft `RLIMIT_NPROC` of every
  container process to `kern.maxprocperuid` less a reserve of an eighth of
  it, at least 64 (`runtime.ForkCeiling`), so a fork bomb fails in the
  kernel while the user's own programs can still start.
- **QoS clamp.** Every container process runs at most at the quality of
  service `fwsd -qos-clamp` names (`utility` by default; `none`,
  `background` or `maintenance`), so containers yield the CPU and the disk
  to the user's interactive work.
- **Cpusets.** `--cpuset-cpus` holds the container to as many CPUs' time as
  the set names, through the supervisor's CPU quota. A set of efficiency
  cores alone (numbered after the performance cores, as `hw.perflevel0`
  and `hw.perflevel1` count them) also gets the background clamp, which
  macOS schedules on efficiency cores; the stronger of it and the engine's
  clamp holds.
- **Exec anchor.** Seatbelt lets a process signal and inspect only processes
  that inherited its sandbox; a process that enters the profile by itself
  is a sandbox of its own. So the main process's spawn helper, once
  sandboxed, starts the container's anchor, a process of `fws-shim`,
  through a child that exits at once, so the anchor is no child of the main
  process. The anchor accepts on a socket in the engine's run directory,
  out of every container's reach, and starts each `docker exec` and health
  check in the container's sandbox with the streams the engine passes,
  reporting its pid and exit status. It carries none of the container's
  environment, so limits and `top` leave it out, and exits with the main
  process. Every exec gets the main process's host pid as `FWS_INIT`, as
  the main process does, and the library presents that process as pid 1.

#### Supervising a container

- **Supervisor.** Each container runs under a shim, `fws-shim` in its
  supervisor role, in a session of its own (each stack's egress proxy is
  `fws-shim` too). The shim owns the container's output and json-file log,
  its pty, its published-port proxies and its exit status, and answers the
  engine over a Unix socket in `<root>/run`. It tracks the container's
  processes: the main process group, descendants found through kqueue
  `NOTE_FORK` events and the forking process's child list, the processes
  macOS holds the main process responsible for, and anything carrying the
  container's `SANDBOX_ROOT` tag; it reads the whole process table only
  when a member forked and ended before its children were read, or an exec
  starts. With a limit set it samples the members every 100 ms, the
  kernel's default CFS period, and enforces:
  - `--memory`: the sum of the processes' physical footprints, which counts
    compressed and swapped-out memory, so `--memory-swap` may only equal it;
    past the limit the largest process is killed and the container reports
    `OOMKilled`. The kernel also holds each program to the whole limit
    through jetsam's fatal footprint limit (`posix_spawnattr_setjetsam_ext`),
    which can be set only on a program being started or executed and
    survives no fork or exec. So the spawn helper sets it on the command
    and the anchor; the library sets it on each `posix_spawn` and runs each
    `execve` as a `posix_spawn` with `POSIX_SPAWN_SETEXEC`, reading the
    limit from the container's `memory.max`; and the anchor and
    `docker-init`, Go programs the library does not load into, start
    programs through `fws-shim`'s `fws-memexec` role. A process forked
    without an exec is held by the shim's sum alone. `NOTE_EXIT_MEMORY`
    marks a kill at the limit, which the shim counts as an OOM kill. No
    process may change a running process's limit, so `docker update`
    cannot raise a running container's memory limit.
  - `--cpus` / `--cpu-quota`: the processes' CPU time against the quota; a
    run over it stops them with `SIGSTOP` until the quota has paid it back,
    and the throttled periods and time show in `docker stats`.
  - `--pids-limit`: the engine writes the limit to the container's
    `pids.max`, rewritten on `docker update`; before a `fork` or
    `posix_spawn` the library asks the shim how many processes the
    container runs and fails the call with `EAGAIN` at the limit.
    Processes past the limit, from racing forks or a program without the
    library, are killed as they appear.

  `docker update` changes these live; `docker pause` stops the processes
  with `SIGSTOP` through the shim, so throttling never continues them.
- **Shared PID namespaces.** A sandbox takes in no other's processes, so
  `--pid container:<name>` shares a namespace through the shims (package
  `pidns`). A file in the owner's state holds the `SANDBOX_ROOT` tags of the
  owner and of each container that joined while it ran (`FWS_PIDNSFILE`).
  Each shim answers its library on a Unix socket (`FWS_PROCS`): it lists
  the namespace's processes by their tags, signals them and reads their
  `proc_pidinfo`, `proc_pidpath` and `kern.procargs2`, and refuses any
  other process with `ESRCH`. A signal or read Seatbelt refuses with
  `EPERM` goes through the shim. A joining container presents the owner's
  main process as pid 1 (`FWS_PIDNS=<its host pid>`). When the owner's
  main process ends, its shim kills the processes of every container the
  file lists, as a PID namespace ends with its init.
- **Network I/O.** The shim connects to macOS's unprivileged network
  statistics control socket (`com.apple.network.statistics`,
  `internal/netstat`). The kernel announces every TCP and UDP socket as it
  opens; the shim asks which pid last used it and keeps watching only
  sockets of a member, of a process with the container's tag, or of the
  router's port. Traffic between two of the container's own sockets counts
  for neither direction.
- **Root size quota.** `--storage-opt size` puts the container's root on a
  sparse APFS disk image (`root.sparseimage`), created with `hdiutil` and
  attached at the root's path, unprivileged; `ditto` copies the image into
  it, keeping extended attributes and flags. The engine attaches it again
  as it loads its containers and detaches it as it removes one.
- **Machine guards.** With `-machine-guard` (on by default) the engine
  reads every 2 s macOS's memory pressure level
  (`kern.memorystatus_vm_pressure_level`) and the free space of the disk
  holding its state (critical under 2 GiB, normal from 4 GiB). While one is
  critical it pauses, one a step, the running container using the most of
  it (the shim's footprint, or bytes written), and resumes those it paused
  once it is normal, so their work waits instead of being lost.
- **Live restore.** The shims and egress proxies are processes of their
  own, so with `-live-restore` the engine stops without stopping its
  containers and the next engine reconnects to the shims. Without it, an
  engine stops its containers as it stops, and ends any a crashed engine
  left running as it starts.
- **Nested engines.** A privileged container gets an engine of its own, as
  a `docker:dind` container runs dockerd (`internal/engine/nested.go`): an
  `engine.Engine` with its state under the container's directory, a range
  of the outer allocator's port blocks, and its API on a socket in the
  outer run directory (`FWS_ENGINE`), which the container's `dockerd`
  relays from the listeners its flags name. macOS refuses a second Seatbelt
  profile to any sandboxed process (`TestSecondSandboxRefused`), so the
  nested engine's containers are the outer engine's processes, each in a
  sandbox of its own that denies the outermost engine's state. The nested
  engine reads a bind mount's source through the container's root and
  mounts, and publishes a port on the container's host port, or on the
  Unix socket in its port directory. A container it runs on the host's
  network, as a kubelet's static pods are, takes the privileged
  container's network (`netOf`). The nested engine and its containers end
  with the privileged container.
- **Capabilities.** A container runs as the engine's unprivileged user, so
  Docker's default capabilities grant it nothing to give up: `--cap-drop` is
  accepted, and `--cap-add` beyond the default set is refused but for
  `IPC_LOCK`: macOS does not enforce `RLIMIT_MEMLOCK`, so any process may
  lock memory within the system's wired-memory limits. For the same reason
  `--ulimit memlock` is accepted only as unlimited. `no-new-privileges`
  holds for every container.

#### The container library

Every container process loads `fwsroot` (`internal/pathmap/src/fwsroot.c`),
injected by the spawn helper with `DYLD_INSERT_LIBRARIES` after it applied
the profile, so no Apple binary sits between them to drop the variable. It
interposes libSystem's path, identity, user database, hostname, socket and
process calls to present the container's root at `/`, its user, its
`/etc/passwd` and `/etc/group`, its hostname, network and processes:
fakechroot and fakeroot in one, presented and not enforced, as Seatbelt
still decides every access by the engine's user. It hides the variable that
loads it from `getenv`, and restores it, with the other `FWS_*` and
`SANDBOX_*` variables, to a program that empties its environment before
executing another. It presents `PATH` and `HOME` as the container's own and
hands a program it starts the root's (`present_env`, `child_env`). Its
networking is §6's.

- **Paths.** Mounts sit in the root as symlinks to their sources, listed in
  `FWS_MOUNTS`, so the library resolves a mount's destination straight to
  its source and reports the source's paths as the destination's. It
  resolves symbolic links itself wherever the kernel would meet one, which
  it learns from `AT_SYMLINK_NOFOLLOW_ANY`: an absolute target starts again
  at the container's `/`, and `..` stops there, as under chroot. The host's
  paths above the root and the mounts' sources stay the host's, and so do
  the trees the loader and libSystem read on every start, which no image
  holds (`host_trees`: `/System`, `/usr/lib`, `/dev`, `/private/var/db`,
  `/var/db`, `/Library/Apple`); `fws-mkimage` refuses an image path in one,
  which no container would see. A path the root lacks, down to its
  parent, is the host's only where the container may read that, as a
  shell or the time zone a root lacks; else it stays the root's and fails
  with `ENOENT`, as under chroot, not with Seatbelt's `EPERM` (`place`).
  A Unix socket's address maps as a file
  path does; a host path past the address's 104 bytes is used relative to
  the root, or the mount holding it, as the thread's working directory
  (`pthread_fchdir_np`). `confstr`'s per-user directories name the
  container's `/tmp`.
- **Owners and modes.** A file presents the owner and mode its image or
  `docker cp` gave it (`own`). On disk the owner's read and write bits, and
  a directory's search bit, are always set; the mode widened is recorded in
  `com.fws.mode`, an owner other than root in `com.fws.owner`, and the file
  marked `UF_NODUMP`, so `stat` reads the attributes only of marked files.
  An unmarked file in the root is root's; an unmarked file under a mount,
  the host's own files under a bind or volume, is the looking user's, as a
  Mac's shared files are in Docker's VM, so a workspace is its user's and
  Git accepts it. `chown` records an owner; `chmod` drops the recorded mode.
- **Identity and IPC.** `setuid` and its kin switch the presented identity,
  `setgroups` and `initgroups` the supplementary groups (`FWS_GROUPS`), and
  every program a process executes inherits that identity, so an
  entrypoint drops root as the official images' do with `gosu`
  (`sandboxenv.Become`). Mach bootstrap names a container registers, and
  POSIX shared memory and semaphore names, carry its IPC prefix, under
  which alone Seatbelt lets it register and look up its own services; a
  lookup finds the container's service first, then the host's. A
  registration for the host's power notifications, which the IOKit denial
  refuses, succeeds and never fires. `sendfile`, which the kernel refuses
  under a profile, becomes a copy.

---

## 4. Image and root filesystem

### Layout

All state lives under one root, `~/.fws` by default (`fwsd -root`), resolved
through symlinks at start-up because Seatbelt matches resolved paths.

```
~/.fws/
  docker.sock                      # the Docker Engine, libpod and CRI APIs
  alloc.json                       # per-stack port blocks and assignments
  networks.json  volumes.json  pods.json
  lib/                             # fwsroot and the spawn helper, built by clang
  run/                             # per container <id12>: .sock (shim),
                                   #   .anchor, .pids, .procs, .engine,
                                   #   .p/ (port directory); egress-<stack>
                                   #   .json/.key/.log
  images/images.json               # image records: tags, config, manifest
  images/oci/                      # an OCI image layout: oci-layout,
                                   #   index.json, blobs/sha256/
  images/layers/<diffid>/          # each layer, unpacked once
  images/roots/<config-digest>/    # each image's root, its layers laid over
  stacks/<stack>/network           # the stack's network file (FWS_NETWORK),
                                   #   with .allow and .rates beside it
  stacks/<stack>/c/<id12>/
    r/                             # the container root: an APFS clone
    init/                          # the root as create left it, for diff
    config.json  exit.json  log.jsonl
    profile.sb  profile.bin        # the profile's text and its bytecode
    pids.max  memory.max  iopolicy # limits the container's processes read
    pidns  shm                     # a shared PID namespace's file, the
                                   #   shm ledger
    root.sparseimage               # the root's disk image (--storage-opt size)
    engine/                        # a privileged container's nested engine
  volumes/<name>/_data/            # named and anonymous volumes
  staging/  trash/                 # roots being cloned or deleted
  buildcache/                      # the classic builder's step cache
  buildkit/                        # BuildKit's content store, snapshots, metadata
```

Cloning and deleting a root takes as long as the image has files, so the
engine clones into `staging/` and deletes from `trash/` without holding its
lock, and moves a root in or out of place under the lock with a `rename`. At
start-up it deletes, in the background, whatever an earlier engine left in
either.

### Images

Every image is an OCI image in `images/oci`, whose `index.json` names each
tagged image as containerd and `docker save` do. Pulled and loaded images
keep their manifests, configs and layers byte for byte, so their digests and
IDs (the config digest, as in Docker) survive a save, load, push or pull.
Removing an image removes the blobs no other image refers to.

The store can also stand on a directory of image archives (`fwsd
-image-archives`; `internal/image/archives.go`). It reads each archive's
names on opening and loads an archive the first time one of its names is
asked for, so a catalog costs only the images a session uses and works
offline. An image removed after its archive loaded stays removed.

Registry credentials come from the client: `docker login` sends them to
`POST /auth`, which checks them against the registry, and the CLI sends them
with every pull and push in `X-Registry-Auth`, and with a classic build in
`X-Registry-Config`. The engine keeps none.

Besides tags, an image carries repository@digest references
(`RepoDigests`): the digest a pull resolved and the manifest digest a push
wrote. A pull of a reference the store already holds reports `Image is up to
date`; otherwise it downloads only the layers the store lacks, three at a
time. A push checks each blob once, mounts those another repository of the
registry holds, and puts the manifest last. `docker load` verifies each blob
against its digest. The store sweeps what a crash left as it opens.

### Layers and roots

Each layer is unpacked once, by its diff ID, into `images/layers/<diffid>`,
holding its entries alone with its whiteouts as files that name them, and
checked against the config's `rootfs.diff_ids`. An image's root,
`images/roots/<digest>`, is its layers laid over an empty tree in order: a
directory the tree lacks comes as one `clonefile` of the layer's subtree, so
images that share layers share their blocks; a layer with a hard link into a
lower layer is applied in place instead. On create, the container gets an
**APFS clone** of the image's root (`clonefile(2)`): copy-on-write, constant
time, and near zero bytes until the container writes, with no union mount
and no root.

Extraction keeps each entry's modification time and records what an
unprivileged process cannot set (owner and mode, §3a). It skips device
nodes (the host's `/dev` serves containers) and refuses entries that would
land outside the root. The layers the engine writes — `docker commit`,
BuildKit's, `export` and `cp` — carry the recorded mode and owner, root
where none is recorded, never the host's user.

### The "rooted at a directory, not at /" contract

A container's paths live under a root the engine names in `SANDBOX_ROOT`.
The engine maps `PATH` and `HOME` (the user's home in the container's
`/etc/passwd` where nothing names one) into the root for programs without
the library; the library presents the container's own. An image's
entrypoint may map other paths with `internal/sandboxenv.Path`. `argv[0]` is
resolved inside the root.

A mount — a named volume, an anonymous volume from the image's `VOLUME`, a
bind or a tmpfs — becomes a symlink at its destination inside the root that
points at the backing host directory; the kernel follows it, and Seatbelt
checks the resolved path, so the profile grants the backing directory. A new
volume first receives the image's content at its mount point, as Docker's
copy-up does. A tmpfs mount's `uid` and `gid` are recorded as its owner
(`image.RecordOwner`). A `noexec` mount's directory is left out of the
profile's `process-exec` allowance, and Docker's `ReadonlyPaths` and
`MaskedPaths` become denials of writes, and of reads and writes, at the end
of the profile.

---

## 5. Per-stack resource assignment

A **stack** is one Compose project: the `com.docker.compose.project` label. A
container without one is a stack of its own (`run-<id>`).

1. **Ports.** Each stack holds a contiguous block of host ports
   (`-port-base`, `-port-limit`, `-block-size`; 20000–29999 in blocks of 100
   by default). Each exposed container port gets the lowest free port of the
   block the host can bind, keyed by service, replica number and container
   port, so a recreated container keeps its port. The block also holds the
   stack's routers, flow ports and egress proxy.
2. **Paths.** Each stack gets `stacks/<stack>/`. No path outside it, other
   than the container's own volumes, appears in its containers' profiles.
3. **Release.** When a stack's last container goes, its block and directory
   are freed.

Assignments persist in `alloc.json`, so a restarted engine hands out the same
ports.

---

## 6. Networking without root or a network stack

Seatbelt allows a container to bind its own host ports on loopback, to
connect to those, its router and its stack's egress proxy, and to send UDP
datagrams to its stack's block. Everything else the network presents is the
container library's and the routers' work.

- **Ports.** `FWS_PORTS` lists the container's ports and their host ports.
  A bind to the wildcard or the container's own address on a container port
  binds the host port on `127.0.0.1`; a TCP bind to loopback alone, or on a
  port without a host port, listens on a Unix socket in the container's
  port directory, which only its own profile reaches, and reads as the TCP
  socket the program made. `getsockname`, `getpeername`, `accept` and
  `recvfrom` report container ports and the address the program bound. A
  bind to port 0, which Seatbelt refuses, takes a free port of the
  ephemeral range, 49152 to 65535. An IPv6 socket bound or connected to an
  IPv4 address, or bound to the dual-stack wildcard, becomes an IPv4 socket
  whose addresses read as IPv4-mapped, because Seatbelt does not match
  `::ffff:127.0.0.1`. `SANDBOX_PORT_<n>` names the host port of port `<n>`,
  and `SANDBOX_<SERVICE>_PORT_<n>` with `SANDBOX_<SERVICE>_HOST=127.0.0.1`
  each peer's, which the library carries to the peer through the router.
- **Refused ports.** A TCP connection to any other loopback port, the
  host's, fails with `ECONNREFUSED` (`refused`), as in a network namespace
  where nothing listens there, rather than with Seatbelt's `EPERM`, so a
  program that retries until its server comes up keeps retrying.
- **Addresses.** Each member of a stack has a member address in
  `127.88.0.0/16`. A network created with an IPv4 subnet (outside
  `127.0.0.0/8` and other networks' subnets) gives each endpoint the
  address its IPAM configuration names, or the lowest free one. A network
  with IPv6 holds an IPv6 subnet too, its own or the lowest free `/64` of a
  unique local `/48` derived from the state directory. Each stack's network
  file (`FWS_NETWORK`), the only part of the engine's state a container
  reads, holds each member's address, names (container name, service,
  hostname, DNS names and aliases), ports' host ports, and addresses on
  networks with subnets. The library resolves a member's names to its
  address on the first network the two share and treats an unheld address
  of a subnet as unreachable. A member's IPv6 address is reached through an
  IPv4 socket to its member address that reads both ends as IPv6. The
  routing sysctl and `SIOCGIF*` ioctls show the host's loopback, then
  `eth0` onwards, one per address on the container's networks; its routes
  are loopback's, a route to each IPv4 subnet on its interface, and a
  default route through the first interface's gateway.
- **Routers.** Each shim runs its container's router on a port of the
  stack's block (`internal/router`). A request is a byte naming its kind,
  then what the kind names. The library connects to a member's address, or
  a member's host port, through its own router with a `StreamRelay`
  request carrying the router's relay key, which lies in the port
  directory; the router carries it to the member's host port, or the
  member's router for a port without one, only when the two share a
  network or a link. Network membership is therefore enforced, not
  presented. A connection to a port of the container's own without a host
  port goes through its router too, which writes the client's address
  before the stream, and the library reports it on `accept`. A UDP socket
  on a port without a host port is a Unix datagram socket; the router
  binds a flow port of the stack's block for it and relays datagrams with
  a six-byte address header, which the library adds and strips.
- **Networks.** User-defined networks decide which members a container
  resolves and reaches. Network mode `container:<id>` takes that
  container's network wholesale: ports, member address, network file, port
  directory, router and egress proxy, in the library's variables and the
  profile alike. `host` is refused but for a nested engine's containers.
  Only the `bridge` driver and the default IPAM driver are accepted, with
  `enable_icc`, `enable_ip_masquerade` and `host_binding_ipv4`.
- **Shared networks.** Stacks whose containers share a user-defined network
  keep their blocks and profiles. Member addresses are unique across
  stacks, and a stack's network file also lists, as foreign, the other
  stacks' members on a network one of its members is on, reached through
  the router's relay. For UDP the router hands out a relay port of the
  stack's block and sends each socket's datagrams on from a port leased
  from the member's block, so the member may answer.
- **Name resolution.** `getaddrinfo` and `gethostbyname` read the
  container's `/etc/hosts` first, then the stack's names, then resolve as
  the host does. Network.framework resolves through `dnssd_getaddrinfo`,
  which asks mDNSResponder over XPC, out of a container's reach: the
  library takes the lookup and hands its handler results of its own;
  `connectx` is routed as `connect`. A query a program sends itself to port
  53 of an outside server goes to the container's router, which answers
  DNS on UDP and over TCP (`DNSStream`): the stack's names itself, any
  other passed to the container's `resolv.conf` servers with internet
  access and refused without.
- **Published ports.** `ports:` entries bind real host listeners in the
  container's shim (`internal/portproxy`), outside the sandbox, forwarding
  to the container's host port or the Unix socket in its port directory. A
  taken host port fails the start with Docker's "port is already
  allocated"; a host port range binds its first free port. UDP publishes as
  `docker-proxy` does: one upstream socket per client, on a port of the
  stack's block leased for the flow's life, ending after 90 seconds idle.
  A container's `fws.ingress.allow` label, addresses and prefixes, makes
  the port proxy close a connection, or drop a datagram, from any other
  source.
- **Egress.** Seatbelt can allow loopback ports or every destination,
  nothing in between, so containers reach the internet through a proxy the
  engine runs for each stack on a port of its block (`internal/egress`):
  HTTP, and on the same port SOCKS5 (CONNECT only). A container's proxy
  URLs carry its member address as the user and an HMAC-SHA256 of it under
  a per-stack key as the password; the proxy answers a wrong pair with 407,
  or SOCKS5's failure status, and resolves members as that container sees
  them. Containers and build steps receive `HTTP_PROXY`, `HTTPS_PROXY`,
  `ALL_PROXY` (`socks5h://`) and `NO_PROXY`, which leaves loopback and the
  member block to the library. The proxy resolves each destination once,
  refuses it if any address is loopback, link-local, multicast, unspecified
  or the host's own, and dials the address it checked, so DNS cannot rebind
  a name onto the host. Containers with `network_mode: none`, or only on
  `internal: true` networks, get no proxy.
- **Egress allow lists.** A container's `fws.egress.allow` label and its
  networks' driver option of that name list hosts, wildcard domains,
  addresses and prefixes, each with an optional port
  (`internal/egress/allow.go`); the engine writes them beside the network
  file (`network.allow`), and the proxy refuses a destination off any list
  that applies: `403 Forbidden` over HTTP, "connection not allowed by
  ruleset" over SOCKS5. While any member has a list, the proxy refuses a
  client without a member's credentials.
- **Bandwidth.** A container's `kubernetes.io/egress-bandwidth` and
  `kubernetes.io/ingress-bandwidth` labels (a pod's annotations, which the
  CRI copies onto its sandbox) are token buckets in bits a second
  (`internal/bandwidth`): its router holds the streams it carries for the
  container, and the egress proxy, reading the rates beside the network
  file (`network.rates`), holds each member's upstream connections.
- **ICMP.** The library makes an unprivileged datagram ICMP socket, as
  `ping` opens, a Unix datagram socket pair, and answers an echo to
  loopback, the container's own address or a member it sees itself. An
  IPv4 echo to a host outside the machine goes to the router as an `Echo`
  request; the router pings through macOS's unprivileged ICMP socket for a
  container with internet access, and refuses after 2 s without a reply.
  Any other echo fails with `ENETUNREACH`.

---

## 7. Images: a curated, Nix-built catalog

The catalog is built from nixpkgs for `aarch64-darwin` by
`scripts/build-images.sh` and `cmd/fws-mkimage`, which relocates each Nix
closure so it loads by relative paths and writes an OCI image with a layer
per store path; `internal/catalog` lists it, for `fws catalog` and CI's
publish check. [IMAGES](IMAGES.md) describes the catalog, its build and its
publication. Compose files name these images explicitly; the engine never
substitutes one image name for another. macOS allows only 87 System V
semaphores machine-wide by default, so the Postgres entrypoint keeps each
server's process limits small.

---

## 8. Docker Engine API surface

Served on `unix://~/.fws/docker.sock` at API version 1.52 (minimum 1.24),
with or without the `/v1.xx` prefix:

- `GET|HEAD /_ping`, `GET /version`, `GET /info`, `GET /events`,
  `GET /system/df` (in API 1.52's shape: image sizes counting each layer
  once, containers' writes, volume usage, and both builders' caches).
  `/info` reports the OSType `darwin`, and a blank one to Testcontainers
  for Java (`tc-java/` user agent), which refuses any engine but Linux's
  and accepts a blank one.
- Containers: `create`, `json` (list, with Docker's filters, and sizes with
  `size`), `{id}/json`, `{id}/changes` (`docker diff`), `start`, `stop`,
  `restart`, `kill`, `wait` (all three conditions), `rename`, `resize`,
  `logs` (follow, tail, since, until, timestamps), `attach` (hijacked,
  multiplexed or raw) and `attach/ws`, both ending on a terminal at the
  client's detach keys, `archive` (`HEAD`, `GET`, `PUT`: `docker cp`),
  `top`, `stats` (streamed every second, or one sample), `pause`,
  `unpause`, `update` (restart policy and resource limits), `export`,
  `DELETE`, `prune`
- Exec: `POST /containers/{id}/exec`, `POST /exec/{id}/start` (attached,
  detached, TTY), `/exec/{id}/resize`, `GET /exec/{id}/json`
- Images: `json`, `create` (registry pull for `darwin/<arch>`, or `docker
  import`), `load`, `{ref}/json`, `{ref}/tag`, `{ref}/history`, `get` and
  `{ref}/get` (`docker save`), `{ref}/push`, `search`, `prune`, `DELETE`;
  references may be repository@digest
- Registries: `POST /auth` (`docker login`), `GET /distribution/{name}/json`
- `POST /commit` (with `changes`, pausing by default)
- Networks: `json`, `create`, `{id}`, `connect`, `disconnect`, `prune`, `DELETE`
- Volumes: `json`, `create`, `{name}`, `prune`, `DELETE`
- Builds: `POST /build`, `POST /build/prune`, `POST /grpc`, `POST /session`
- The engine's own: `GET` and `PUT /fws/networks/{name}/services`, a
  network's virtual services (Kubernetes below)

Create writes the container's `/etc/hosts`, `/etc/hostname`,
`/etc/resolv.conf`, `/proc` and mount points, and an `/etc/os-release`
(`ID=darwin`) where the image and its mounts give none. `init/`, a second
clone of the image's root brought to the container root's state at those
paths alone, holds what Docker's init layer holds; `docker diff` and a
container's size compare the root against it.

Health checks run on the image's or container's own cadence and report
Docker's states and events; they and `exec` wait while a container is
paused, and stopping a paused container resumes it to deliver the stop
signal. Restart policies follow Docker's back-off.

Requests the engine cannot honour return an error, `501` for a knob only a
kernel or root could enforce: a swap limit apart from the memory limit,
memory reservations, swappiness and disabling the OOM killer; a cpuset
past the Mac's CPUs or memory node, a running container's new cpuset,
real-time CPU scheduling and per-device block I/O limits; the `nproc`,
`data`, `as`, `rss` and similar ulimits, and a finite `memlock`; devices,
cgroups, sysctls, user namespaces and storage options but `size`; a
capability beyond Docker's default set but `IPC_LOCK`; seccomp, AppArmor and
SELinux options but `unconfined` and `disable`; `host` networking outside a
nested engine; non-`bridge` network drivers; IPAM drivers other than the
default; and SCTP publishing.

`top` reads the host's process table through `ps`, as dockerd does, keeps
the container's processes (§3a's members, but the exec anchor) and presents
the host paths in their command columns as the container's
(`procs.PresentPaths`). `stats` sums the same processes' CPU time;
`system_cpu_usage` is the machine's CPU time elapsed since the engine
started. Memory, CPU throttling, block I/O, network counts and the pids
count come from the shim.

`docker cp` resolves paths as the container's processes do, and refuses any
result outside the container's root and mounts. It archives mounts as their
contents rather than as the symlinks that implement them, and into a
`--read-only` root writes only on read-write mounts.

### BuildKit

`POST /grpc` and `POST /session` serve an embedded BuildKit
(`internal/buildkit`), as dockerd does, so buildx's docker driver, `docker
build` and `docker compose build` reach the real BuildKit solver and its
Dockerfile frontend. Its darwin worker supplies what is otherwise
Linux-specific:

- **Snapshotter.** A containerd snapshotter over directories: a new snapshot
  is an APFS clone of its parent, and every snapshot is a read-write bind
  mount, which BuildKit's darwin mounter uses without `mount(2)`. It
  unpacks layers as the image store does, and measures a snapshot by what
  it adds, when BuildKit first asks.
- **Executor.** Each process runs under a Seatbelt profile confined to its
  root snapshot and mounts. BuildKit's mounts (cache, bind, secret, tmpfs)
  are symlinks at their destinations for the run only. Network is the
  build's egress proxy, or none. Container uid and gid 0 map to the
  engine's user, as in rootless BuildKit.
- **Images.** `FROM` resolves the engine's images first. The `moby`
  exporter (`docker build`, `--load`) writes the result as an OCI image
  extending the base's layers, stores it in the engine's image store, and
  clones the result's snapshot as its root.

The BuildKit packages the engine uses live in `third_party/buildkit`, copied
by `scripts/sync-buildkit.sh` with their imports rewritten into this module,
and the patches in `third_party/buildkit-patches` applied:

- `localmounter_darwin`: a read-only snapshot is read from its directory
  instead of mounted with `mount(2)`, which needs root.
- `ssh_socket_owner`: a step's SSH agent socket stays the worker's user's,
  which stands for the step's root.
- `syntax_builtin`: a `# syntax=` directive naming Docker's own Dockerfile
  frontend, a Linux image, is served by the built-in frontend.
- `resolvecache_race`: the frontend's resolve cache is set up once, before
  stages resolve their bases concurrently.
- `session_conn_closed`: a session whose one connection fails ends at once
  rather than failing its health checks for seconds after.

### The classic builder

`POST /build` is the classic builder (`internal/dockerfile` parses; the
engine builds), for `DOCKER_BUILDKIT=0`. Each stage starts from an APFS
clone of its base image, or an empty root for `scratch`. `COPY` and `ADD`
resolve paths inside the stage root as a chroot would, reading the uploaded
context or, with `--from`, an earlier stage or image. `RUN` executes under
a Seatbelt profile confined to the stage root, with internet only through
an egress proxy on a stack the build owns. Each `RUN`, `COPY` and `ADD`,
and a `WORKDIR` that makes its directory, adds a layer: the difference
between an APFS clone of the stage root taken before the step and the root
after it. `docker commit` adds one layer the same way.

Each step is cached under a key that chains every instruction from the base
image's ID; a `RUN` also folds in the build arguments in effect, and a
`COPY`, `ADD` or directory-making `WORKDIR`, which are cheap, runs and folds
in what it changed (names, modes, links and contents, not times). The cache
keeps each step's first history time, so a rebuild that hits on every step
makes the same image. `--no-cache` skips lookups; `docker image prune` and
`POST /build/prune` empty the cache. With `remote`, the engine fetches the
context itself: a Git repository at the fragment's ref and subdirectory, a
tarball, or a plain Dockerfile.

### Podman's API

The socket serves Podman's API beside Docker's, under `/libpod`, versioned
by Podman's releases (`internal/api/libpod*.go`). Endpoints that speak
Docker's protocol (attach, exec, kill, pause, rename, resize, archive) pass
to Docker's handlers; the rest take and give Podman's shapes: a
SpecGenerator becomes the equivalent Docker create request, list, inspect,
remove and wait answer as Podman's do, logs come framed by stream even from
a terminal, and pulls stream Podman's progress lines. Pods are a record
each in `pods.json` and Podman's `io.podman.pod` label on each member.
`kube play`, `kube generate` and `kube down` run, write back and remove
Pods and Deployments.

### Kubernetes

- **CRI.** The socket also serves the Kubernetes Container Runtime
  Interface (`internal/cri`): a request over unencrypted HTTP/2 with a gRPC
  content type goes to it. It maps CRI onto the engine as cri-dockerd maps
  it onto Docker. A pod sandbox is a pause container (`fws/pause:3.10.2`)
  holding the pod's network, ports and DNS, which the pod's containers join
  as `--network container:` does. Every pod not on its node's network joins
  the network `fws-pods`, whose subnets are the pod CIDR the kubelet sends,
  or kind's `10.244.0.0/16` until it sends one. The engine copies each
  container's output to the kubelet's log file in CRI's format, and serves
  exec, attach and port-forward streams on a loopback port.
- **Nodes.** A kind node is a privileged container whose nested engine
  (§3a) is its kubelet's runtime. The CRI reads the node's containerd
  configuration, which kind patches (`internal/containerd`), at each pull
  and pulls through the registry mirrors and hosts it names, dialling them
  on the node's networks (`Engine.DialNode`); any other setting leaves the
  runtime not ready.
- **Cross-node pods.** Nested engines in privileged containers that share a
  network are a cluster's nodes (`internal/engine/peers.go`). Each publishes
  its members on networks with subnets to the outer engine, which hands
  them to the nested engines of the containers its container shares a
  network with; each lists them in its network files as foreign members,
  reached through their routers. A pod on an IPv6-only network is listed
  under a key of its own in the member block (`peerKeys`). The outer engine
  refreshes the others from a goroutine of its own, and takes no nested
  engine's lock while holding its own.
- **Services.** A network can carry virtual services
  (`internal/engine/services.go`), as Kubernetes Services' cluster IPs: one
  or two cluster IPs and external IPs, names (`name`, `name.namespace`,
  `name.namespace.svc`, `name.namespace.svc.cluster.local`), TCP and UDP
  ports with their endpoints, session affinity, or an external name. The
  engine serves each service port on a loopback port outside any sandbox
  and carries each connection, or UDP flow, to the next endpoint, as
  kube-proxy's userspace mode does; a port with a node port also answers
  on the node's port of that number. The network file lists services as
  members of the stack `services`. `SetServices` replaces a network's
  services all at once or not at all: it opens every new listener and node
  port before it closes any, so an update that fails, on a node port
  another process holds, leaves services, listeners and node ports as they
  were.
- **fws-kube-proxy.** `cmd/fws-kube-proxy` is the node's kube-proxy: it
  polls the API server for Services and EndpointSlices (`-interval`, 1 s),
  with no client library, and `PUT`s the whole list to
  `/fws/networks/fws-pods/services` on the engine's socket (`FWS_ENGINE`)
  when it changes and every 30 s (`-resync`), so a restarted engine gets it
  back.

---

## 9. Runtime launch sequence

On create: clone the image root, assign the stack's ports, realise the
mounts, and record the container. On start:

1. Render the Seatbelt profile from the container's grants to
   `profile.sb`, outside the root, and compile it to `profile.bin` unless
   the bytecode of the same text is kept (§3a), alongside step 3.
2. Build the environment: image and container variables, `PATH` and `HOME`
   mapped into the root, and the `SANDBOX_*` and `FWS_*` variables. With
   `--init`, link `fws-shim` at `/sbin/docker-init` in the root and put it
   before the command; run by that name it acts as tini.
3. Start the container's shim (§3a), which binds the published host ports.
4. The shim spawns the helper, which applies the limits and the profile,
   starts the exec anchor, and executes `<root>/<argv0> …` with the
   container library loaded, the main process leading its own process
   group (or session, with a TTY). Output reaches the json-file log through
   pipes, so the shim sees the exit at once even if a descendant still
   holds stdout.
5. When the main process exits, the shim kills the rest of the container,
   drains the output for up to 2 s, records the exit, and tells the engine,
   which applies the restart policy.

A process start costs about 3.4 ms natively to reach `main`, 7 ms with any
library inserted (the loader loses its prebuilt launch), and about 4 ms more
for the functions `fwsroot.c` interposes, some 25 µs each, whichever they
are (`BenchmarkStartCost`, on CI's macOS 26 runner).

---

## 10. Go implementation

The engine cross-compiles with `CGO_ENABLED=0`. What runs inside every
container process start is C, which the engine compiles with clang on the
Mac before its first container starts, into `lib/`: the container library
(`internal/pathmap/src/fwsroot.c`) and the spawn helper
(`internal/pathmap/src/spawn.c`). `clonefile` comes from
`golang.org/x/sys/unix`, image handling from `go-containerregistry`, and the
API types from `github.com/moby/moby/api`, so the JSON matches what Docker
clients decode.

```
cmd/fws/              # the command line: fws catalog lists the image catalog
cmd/fwsd/             # the daemon: Docker Engine, libpod and CRI APIs over a unix socket
cmd/fws-shim/         # the helper binary: supervisors, exec anchors, egress proxies,
                      #   docker-init, fws-memexec
cmd/fws-mkimage/      # Nix closure -> darwin image archive
cmd/fws-kube-proxy/   # kube-proxy for a node whose runtime is the engine
internal/alloc/       # per-stack port blocks and directories
internal/api/         # the Docker Engine API and Podman's libpod
internal/bandwidth/   # token buckets for the bandwidth labels, in routers and egress proxies
internal/buildkit/    # embedded BuildKit with a darwin worker
internal/catalog/     # the image catalog: names, tags, kinds, where each is published
internal/containerd/  # a kind node's containerd configuration: registry mirrors and hosts
internal/cri/         # the Kubernetes Container Runtime Interface
internal/dockerfile/  # Dockerfile parsing and variable expansion
internal/egress/      # the per-stack HTTP and SOCKS5 proxy to the internet
internal/engine/      # containers, volumes, networks, services, exec, health, events, guards
internal/execpath/    # finding a container's executables, as execve and a shell do
internal/identity/    # users and groups from the container's /etc/passwd
internal/image/       # image store, layer extraction, clonefile roots
internal/libsystem/   # libSystem calls syscall does not offer (posix_spawn, I/O policy,
                      #   sandbox_compile_string)
internal/logfile/     # json-file logs
internal/members/     # a stack's network file: member addresses and names
internal/netstat/     # per-socket network counters for docker stats
internal/pathmap/     # fwsroot, the container library, the spawn helper, and their build
internal/pidns/       # PID namespaces containers share, through their shims
internal/portproxy/   # published host ports, TCP and UDP
internal/procs/       # the host process table: the shim's member tracking, top and stats
internal/roles/       # the helper processes the engine's binary runs
internal/rootfs/      # chroot-style path resolution inside a root
internal/router/      # traffic to container ports without host ports, relays, DNS, ICMP echo
internal/runtime/     # starting container processes through the spawn helper; the
                      #   compiled-profile cache; docker-init
internal/sandboxenv/  # the container side of the SANDBOX_* contract
internal/seatbelt/    # Seatbelt profile rendering
internal/shim/        # a container's supervisor: limits, output, live restore
third_party/buildkit/ # the BuildKit packages it uses, patched (sync-buildkit.sh)
images/               # entrypoints and Nix specs for the darwin catalog
tests/e2e/            # Docker CLI, Compose and Testcontainers end-to-end suite (own module)
```

---

## 11. Testing

Every isolation property is proven by a test that tries to break it, on a
macOS runner; no test skips for a missing tool. The Seatbelt and
container-library packages run probes under the real sandbox, and the
end-to-end suite drives the stock Docker CLI, Docker Compose, Podman,
Testcontainers, kind and `crictl` against an engine of its own.
[`CONTRIBUTING.md`](../CONTRIBUTING.md) lists every check CI runs and how to
run each locally; [`LIMITATIONS.md`](../LIMITATIONS.md) lists what the
engine cannot do; [`HISTORY.md`](HISTORY.md) records how it got here.
