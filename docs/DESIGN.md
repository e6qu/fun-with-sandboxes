# fun-with-sandboxes — Design

A no-root, no-VM container engine for macOS that speaks a subset of the Docker
Engine API, good enough to run `docker compose` stacks of macOS-native images
side by side without clashing on ports, paths, or processes, and without
reaching the host beyond what macOS strictly requires.

The engine is written in Go and cross-compiles to `darwin/arm64`: it runs
on Apple silicon alone. It runs the workloads on the host directly — there is no virtual
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

- **Not a multi-tenant or untrusted-code sandbox.** It is a local runtime for
  a developer's tests and services, lighter than a Linux VM; Seatbelt keeps
  stacks apart and the user's data out of reach, against mistakes rather
  than attackers.

- **Not a Linux container runtime.** It runs macOS-native (Mach-O,
  `darwin/<arch>`) images only. Linux ELF images cannot run on macOS without a
  VM or emulation, both excluded. Images come from the Nix-built catalog
  (§7) or from Dockerfiles built on it; [`PLAN.md`](../PLAN.md) covers
  building Dockerfiles written for Linux.
- **Not root-requiring.** No step uses `sudo`, installs a kext, disables SIP,
  adds `lo0` aliases, or writes `pf` rules. If a design choice needs root, it is
  rejected and replaced with an unprivileged equivalent.
- **Not a drop-in for Docker Hub images.** Images are built for this engine (see
  §7). Stock `postgres:16` from Docker Hub is a Linux image and does not apply.

### Accepted exceptions

Where macOS gives an unprivileged process no alternative, a container sees
more of the host than under Docker, though it can act on none of it:

- **Process listing.** A program that loads no container library, as
  Apple's `ps` does not, lists every process of the user by host pid.
  Seatbelt still denies signalling or inspecting another sandbox's
  processes.
- **Shared kernel surfaces.** `sysctl` values, the system clock and the
  Mach host port are global and readable; Seatbelt denies writing them.
- **The host's system tree.** A container reads everything under `/` but
  the user's and the machine's data roots (`/Users`, `/Volumes`,
  `/Library`, `/opt`, `/usr/local`, `/nix`, temporary and per-user
  directories and others, `seatbelt.DeniedReadRoots`) and the engine's
  state, of which it gets back only its own root, volumes and files (§3).

Another stack's files, ports, environment and logs, and the right to signal
its processes, stay denied. [`LIMITATIONS.md`](../LIMITATIONS.md) lists every
gap in full.

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
   distinct host ports and real data paths. The container library binds a
   container port's host port in its place, so a service listens on its
   container port as written; the `SANDBOX_*` variables name the
   assignment for a program that loads no library (§5, §6).

This is the pivot of the project: **isolation by assignment, enforced by
Seatbelt**, not isolation by virtualization. The engine is the authority that
hands out non-overlapping resources; Seatbelt is the fence that stops a
container using anything it was not handed.

---

## 3. Isolation model — what each primitive provides

| Dimension | Primitive | Guarantee | Gap (accepted) |
|---|---|---|---|
| Filesystem write | Seatbelt `(deny file-write*)` + allow only the container root and its volumes | A container writes only inside its own clone and mounts | — |
| Filesystem read of host | Seatbelt `(allow file-read* (subpath "/"))`, then `(deny file-read*)` of the user's and machine's data roots (`seatbelt.DeniedReadRoots`) and the engine's state, then allow the container root, its volumes, the container library and its network file | No read of `$HOME`, other stacks' roots, or user data | The rest of the host's system tree is readable |
| Cross-stack visibility | Per-stack directory tree + per-stack Seatbelt path allowances | Stack A's profile names only A's paths; A cannot read or write B | — |
| Ports | Engine allocates a disjoint range per stack; Seatbelt `(deny network-bind)` except the assigned ports | Two stacks never clash; a container binds only its grants | Host sees all listening sockets globally |
| Process signalling | Seatbelt `(deny signal)` and `(deny process-info*)` except same-sandbox; execs start in the container's sandbox; the container library lists the sandbox's processes alone. `--pid host` allows both, and the library lists every process (`FWS_PIDNS=host`). `--pid container:` shares a namespace through the containers' shims (below) | A cannot `kill`, inspect or list B's processes | pids are the host's (no PID namespace) |
| Default deny | Seatbelt `(deny default)` on Apple's `system.sb` baseline, each operation class re-allowed by name | A container gets no IOKit, Mach registration, preferences, AppleEvents, kernel control sockets or anything else the profile does not name | — |
| IPC | Seatbelt `ipc-posix-name-prefix` filters; the container library's per-container System V keys | POSIX shared memory and semaphores confined to a per-container name prefix (`--ipc` private); System V keys hashed with it; `none` denies all IPC | a System V key is presented, not confined: a guessed hashed key reaches another container's object |
| Resource limits | Per-container supervisor (§3a); `setrlimit`, the nice value, the disk I/O tier and a QoS clamp through the spawn helper | Memory, CPU, pids caps, `--ulimit`, priority and disk I/O tiers | Enforced by ending, stopping or killing processes, not by refusing allocations; the container library refuses forks at the pids limit |
| Identity | Runs as the invoking user; the container library presents the user `--user` or `USER` names, resolved against the container's own `/etc/passwd` and `/etc/group` | Programs see the container's user and groups, as under Docker | Presented, not enforced: every process runs as the invoking user |

Rejections are deliberate: an unsupported knob returns a clear API error rather
than silently doing nothing. Silent no-ops would be a fidelity bug.
[`SEATBELT.md`](SEATBELT.md) shows the profile's sections and how to read a
container's profile and its denials.

### 3a. Isolation and limits without cgroups

macOS gives an unprivileged process no cgroups, no namespaces and no
capability sets. What it does give — `posix_spawn` attributes, `setrlimit`,
`setpriority`, `setiopolicy_np`, `proc_pid_rusage`, kqueue process events,
`SIGSTOP`/`SIGCONT`, and Seatbelt's filters — the engine composes into the
same knobs, each enforced by a real mechanism or refused:

- **Spawn helper.** Every container's main process and every build step
  starts as the spawn helper, a C program the engine builds beside the
  container library (`internal/pathmap/src/spawn.c`), through `posix_spawn`
  with responsibility disclaimed, so the process inherits no privacy (TCC)
  grant of whatever launched the engine. The helper applies `--ulimit`
  (nofile, fsize, cpu, core, stack), the nice value `--cpu-shares` maps to,
  the disk I/O policy tier `--blkio-weight` maps to, the controlling
  terminal and the fork ceiling, applies the container's compiled Seatbelt
  profile (`sandbox_apply`), and executes the command with the container
  library loaded, through `posix_spawn` with `POSIX_SPAWN_SETEXEC`, the one
  exec that takes a QoS clamp. A limit the user cannot set fails the start,
  reported on a close-on-exec status pipe.
- **Compiled profiles.** libsandbox takes about 15 ms to compile a
  profile's text and the kernel about 35 µs to apply the bytecode. The
  engine renders the profile (`internal/seatbelt`) to `profile.sb`,
  compiles it with `sandbox_compile_string` (`libsystem.SandboxCompile`)
  to `profile.bin` beside it, keyed by the SHA-256 of the text and the
  macOS build (`kern.osversion`) (`internal/runtime`), while the shim
  starts, and the helper applies the bytecode. A start whose text is
  unchanged, as a restart's, compiles nothing; an update of macOS
  compiles every profile again. Where macOS refuses the bytecode, the
  helper compiles the text itself through `sandbox_init`, the stable,
  deprecated interface, and the container starts 15 ms later, confined
  the same.
- **Fork ceiling.** The helper lowers the soft `RLIMIT_NPROC` of every
  container process to `kern.maxprocperuid` less a reserve of an eighth
  of it, at least 64 (`runtime.ForkCeiling`), so a fork bomb fails in the
  kernel while the user's own programs can still start.
- **QoS clamp.** Every container process and what it starts runs at most
  at the quality of service `fwsd -qos-clamp` names (`utility` by default;
  `none`, `background` or `maintenance`), set with
  `posix_spawnattr_set_qos_clamp_np`, so containers yield the CPU and the
  disk to the user's interactive work.
- **Machine guards.** With `-machine-guard` (on by default) the engine
  reads two measures every 2 s: macOS's memory pressure level
  (`kern.memorystatus_vm_pressure_level`) and the free space of the disk
  holding its state, critical under 2 GiB and normal again from 4 GiB.
  While a measure is critical it pauses, one container a step, the running
  container that uses the most of it (the shim's footprint, or bytes
  written), as `docker pause` does, and resumes the ones it paused once the
  measure is back to normal. A paused container's memory pages out and it
  writes no more; its work waits instead of being lost.
- **Exec anchor.** Seatbelt lets a process signal and inspect only the
  processes of its own sandbox, the ones that inherited it, and a process
  that enters the container's profile by itself is a sandbox of its own. So
  a main process's spawn helper, once sandboxed, starts the container's
  anchor before it executes the command:
  - The anchor is a process of `fws-shim` that the helper starts through a
    child that leaves at once, so it is no child of the main process, whose
    `wait` would otherwise wait for it.
  - It accepts on a socket the shim binds in the engine's state, which no
    container can reach.
  - It starts each `docker exec` and health check the engine asks for,
    with the streams the engine passes it, in the container's sandbox, and
    reports its pid and exit status.
  - It carries none of the container's environment, so its limits and
    `top` leave it out, and it exits with the main process.
  - It gives every exec the main process's host pid as `FWS_INIT`, as the
    helper gives the main process, and the container library presents
    that process as pid 1.
  - The profile lets the sandbox execute `fws-shim` for it, and nothing
    else beyond the root, its volumes and the system shells.

  Step 4 of [`SEATBELT.md`](SEATBELT.md)'s tutorial shows the sandbox
  inheritance this relies on.
- **Shared PID namespaces.** A running container's sandbox takes in no
  process of another's, so `--pid container:<name>` shares a namespace
  through the shims, outside the sandboxes:
  - The engine keeps the namespace's file in its owner's state, where no
    container writes: the `SANDBOX_ROOT` tags of the owner and of each
    container that joined it while the owner ran, which the engine takes
    out as each ends.
  - Each container's shim answers its library on a Unix socket in
    `<root>/run` that the container's profile reaches by its path alone
    (package `pidns`): it lists the namespace's processes, found by their
    tags, signals them and reads their `proc_pidinfo`, `proc_pidpath` and
    `kern.procargs2`, and refuses every other process with `ESRCH`.
  - The library reads the file (`FWS_PIDNSFILE`); while it lists other
    containers, the library's listings take in the namespace's processes,
    and a signal or a read Seatbelt refuses with `EPERM` goes through the
    shim (`FWS_PROCS`). A joining container's library presents the
    owner's main process as pid 1 (`FWS_PIDNS=<its host pid>`).
  - When the owner's main process ends, its shim kills the processes of
    every container the file lists, as a PID namespace's processes end
    with its init.
- **Supervisor.** Each container runs under a shim, `fws-shim` in its
  supervisor role and a session of its own; each stack's egress proxy is
  `fws-shim` too. The shim owns the container's
  output and json-file log, its pty, its published-port proxies, and its exit
  status, and answers the engine over a Unix socket in `<root>/run`. It
  tracks the container's processes — the main process group, descendants
  found through kqueue `NOTE_FORK` events and the kernel's list of the
  forking process's children, the processes macOS holds the main process
  responsible for, and anything that started with the container's
  `SANDBOX_ROOT` tag — reading the whole process table only when a member
  forked and ended before its children were read, or an exec starts. With
  no limit set it waits on those events alone; with one, it also samples
  the members every 100 ms, the kernel's default CFS period, and
  enforces:
  - `--memory`: the sum of the processes' physical footprints, which counts
    compressed and swapped-out memory, so `--memory-swap` may only equal it;
    past the limit the largest process is killed, as the kernel OOM killer
    picks one in a cgroup, and the container reports `OOMKilled`. The
    kernel also holds each program to the whole limit at once: jetsam's
    fatal footprint limit (`posix_spawnattr_setjetsam_ext`, in MiB), which
    a process may set only on a program it starts or executes, and which
    the kernel keeps across no fork and no exec. So it is set on every
    program: the spawn helper sets it as it executes the command and the
    exec anchor; the container library sets it on each `posix_spawn` and
    executes each `execve` through `posix_spawn`'s `POSIX_SPAWN_SETEXEC`
    with it, reading the limit from the container's `memory.max` beside
    `pids.max`; and the anchor and `docker-init`, Go programs the library
    does not reach, start their programs through the helper binary's
    `fws-memexec` role, which executes them with it. A process forked
    without an exec runs without it, held by the shim's sum alone. The
    shim watches each exit with
    `NOTE_EXIT_DETAIL`, whose `NOTE_EXIT_MEMORY` says the kernel killed the
    process at its limit, and counts that kill as an OOM kill. No process
    may change a running process's limit, not even its own
    (`memorystatus_control` is refused), so `docker update` refuses to
    raise a running container's memory limit.
  - `--cpus` / `--cpu-quota`: the processes' CPU time against the quota; a
    run over it stops them with `SIGSTOP` until the quota has paid it back,
    and the throttled periods and time show in `docker stats`.
  - `--pids-limit`: the engine writes the limit to the container's
    `pids.max`, as a pids cgroup holds it, and rewrites it on
    `docker update`; the container library reads it before a `fork` or a
    `posix_spawn` that starts a process, asks the shim on a socket of its
    own how many processes the container runs, as `pids.current` holds
    it, and fails the call with `EAGAIN` when the container already runs
    that many. The shim answers from the members it follows, dropping
    those that ended and reading each one's children, so the count holds
    a fork made a moment before. Processes past the limit, which
    two racing forks or a program without the library start, are killed
    as they appear.
  `docker update` changes these live; `docker pause` stops the processes
  through the shim so throttling never continues them.
- **Network I/O.** The shim connects to macOS's network statistics control
  socket (`com.apple.network.statistics`, `internal/netstat`), which needs
  no privilege on macOS. The kernel announces every TCP and UDP socket as it
  opens; the shim asks for its report at once and reads the pid that last
  used it from the descriptor, while that process most likely still runs.
  A socket of a member, of a process carrying the container's
  `SANDBOX_ROOT` tag, or of the router's port is the container's; the shim
  stops watching every other. It keeps the final counts the kernel reports
  as each socket closes, and `docker stats` polls the rest. A connection
  between two of the container's sockets crosses no boundary and counts for
  neither.
- **Nested engines.** A privileged container gets an engine of its own,
  as a `docker:dind` container runs dockerd (`internal/engine/nested.go`):
  the outer engine opens an `engine.Engine` whose state is under the
  container's directory and whose allocator holds a range of the outer
  one's port blocks, cut into smaller blocks so it holds forty stacks, as
  a kubelet's node makes one for each pod, and serves its API on a socket
  in the outer run directory (`FWS_ENGINE`), which the container's profile lets it reach
  and its `dockerd` relays from the listeners its flags name. macOS
  refuses `sandbox_init` in a process already under a profile that denies
  anything, so no engine inside the sandbox could confine containers; the
  nested engine's containers are the outer engine's processes, each in a
  sandbox of its own, which deny the outermost engine's state. The nested
  engine reads a bind mount's source through the container's root and
  mounts, and publishes a port in the container: on the host port of the
  container's port, or on the Unix socket its port directory holds for a
  port without one, where the container's library connects to its own
  loopback ports. A container it runs on the host's network mode, as a
  kubelet's static pods are, takes the privileged container's network as
  a container on another's does (`netOf`): the outer engine's network
  file, port directory, router, ports and egress proxy for that
  container, which its profile reaches. It ends its containers when the container ends, and
  goes with the container.
- **Live restore.** The shim and each stack's egress proxy are processes of
  their own, so with `-live-restore` the engine stops without stopping its
  containers, and the next engine reconnects to the shims through their
  sockets. Without it, an engine stops its containers as it stops, and ends
  any an earlier, crashed engine left running as it starts.
- **Container library.** Every container process loads `fwsroot`
  (`internal/pathmap`), a library the engine builds from embedded C source
  and the spawn helper injects with `DYLD_INSERT_LIBRARIES`, having applied
  the Seatbelt profile itself through `sandbox_apply` so no Apple binary
  sits between them to drop the variable. It interposes
  libSystem's path, identity, user database and hostname calls to present
  the container's root at `/`, its user, its `/etc/passwd` and `/etc/group`,
  and its hostname: fakechroot and fakeroot in one, presented and not
  enforced, as Seatbelt still decides every access by the engine's user.
  Mounts sit in the root as symlinks to their resolved sources; the engine
  lists them in `FWS_MOUNTS`, so the library resolves a mount's destination
  straight to its source and reports the source's paths as the
  destination's. The host's paths above the root and the mounts' sources,
  which a program walks when it resolves a `$SANDBOX_ROOT` path, stay the
  host's. The library resolves a path's symbolic links itself wherever the
  kernel would meet one, which it learns from `AT_SYMLINK_NOFOLLOW_ANY`: an
  absolute target starts again at the container's `/`, and `..` stops
  there, as under chroot; a call that follows no final link, as `lstat` and
  `O_NOFOLLOW` do, leaves the last component unresolved. A script whose
  `#!` names `/usr/bin/env` runs the program its argument names from the
  container's `PATH`, as `env` would, because Apple's `env` cannot load the
  library.
  A file's mode is the one its image or `docker cp` gave it: extraction
  keeps the owner's read and write, and a directory's search, on disk,
  which the engine's user and a container's root need, records the mode
  it widened in an extended attribute (`com.fws.mode`), which `stat` and
  the engine's archives report, and which `chmod` drops. The library
  hides the variable that loads it from `getenv`, as no container's
  environment holds it, and restores it to a program that empties it
  before it executes another, as ClickHouse does.
  `setuid` and its kin switch the presented identity, `setgroups` and
  `initgroups` the supplementary groups (`FWS_GROUPS`), and every program
  a process executes inherits the identity it holds then, with the
  container's variables, whatever environment the process passes: an
  entrypoint drops root as the official images' entrypoints do with `gosu`
  (`sandboxenv.Become`), and a program that clears its environment stays in
  the container. A program that registers for the host's power
  notifications, which the sandbox's IOKit denial refuses, gets a
  registration that never fires, as a container has no sleep or wake of
  its own; the library binds IOKit weakly, so it loads it into no process
  that does not. The Mach bootstrap names a container registers, as
  Chrome registers its port rendezvous, carry its IPC prefix, under which
  alone Seatbelt lets it register and look up services of its own; a lookup
  finds the container's service first, then the host's. POSIX shared
  memory and semaphore names get the prefix the same way, so a program
  names them as it would on a host of its own. The engine lists the
  container's ports and their host ports in `FWS_PORTS`: a bind to the
  wildcard or the container's own address on a container port binds the
  host port on `127.0.0.1`, a TCP bind to loopback alone listens on a Unix
  socket instead, out of the published port's reach, as a port without a
  host port does, a `localhost` connection or datagram to a container port
  reaches that listener or else its host port, and `getsockname`, `getpeername`, `accept` and
  `recvfrom` report container ports and the wildcard the program bound. An
  IPv6 socket bound or connected to an IPv4 address, or bound to the
  dual-stack wildcard, gets an IPv4 socket with its settings in the same
  descriptor, whose addresses read as IPv4-mapped IPv6 ones, because
  Seatbelt matches `127.0.0.1` and `::1` but not `::ffff:127.0.0.1`. An
  IPv6 socket connecting to `::1` first tries to bind the host port on IPv6
  loopback, a probe no listener sees: where nothing holds it, the socket
  becomes an IPv4 one too and reaches the dual-stack listener on
  `127.0.0.1`, its loopback addresses reading as `::1`.
  Each member of a stack has a member address in `127.88.0.0/16`, assigned
  as it is created and kept in its record; it is the member's identity in
  the network file. A network created with an IPv4 subnet (one, of the
  default IPAM driver, outside `127.0.0.0/8` and other networks' subnets)
  gives each endpoint an address of it too: the one its IPAM configuration
  names, or the lowest of the range that no endpoint, the gateway or an
  auxiliary address holds, recorded in the endpoint and reported by
  inspect while the container runs. A network with IPv6 holds an IPv6
  subnet beside it, its own or the lowest free `/64` of a unique local
  `/48` the engine derives from a hash of its state directory, as dockerd
  derives its default pool from its identity, and gives each endpoint an
  IPv6 address of it the same way; a container names one only in a subnet
  the network was created with. The engine keeps a network file per
  stack, rewritten as members come, go, are renamed or change networks,
  of each member's address, the names it answers to (its container name,
  service, hostname, DNS names and aliases), its ports' host ports, and
  its addresses on networks with subnets, IPv4 and IPv6,
  `network=address/bits`; the
  container reads it (`FWS_NETWORK`) and nothing else of the engine's
  state. The library resolves a member's names to its address on the
  first network the two share, by name, takes any of a member's addresses
  as the member's, any of its own as its own, and an address of a
  network's subnet that no member holds as unreachable. A member's IPv6
  address maps to its member address, through an IPv4 socket in the IPv6
  one's place, as an IPv4-mapped address does; the socket records that
  it reached a member over IPv6, and reads members, and its own end, at
  their IPv6 addresses on the network it shares with its peer. Over IPv6
  `getaddrinfo` and `gethostbyname2` answer a member's IPv6 address, and
  for any family, sort a unique local one after IPv4 and a global one
  before. It presents the
  container's interfaces where a program lists them, in the routing
  sysctl (`NET_RT_IFLIST`, `NET_RT_IFLIST2`) that `getifaddrs` and Go read
  and in the `SIOCGIF*` ioctls: the host's loopback interface, and `eth0`
  onwards, which take the indexes after the host's highest, one for each
  of the container's addresses on its networks, in their order, with its
  subnet's mask and, on a network with IPv6, its IPv6 address there. A Unix socket's address maps as a
  file path does, in `bind`, `connect` and `sendto`, and reads back as the
  container's path. A host path past the address's 104 bytes binds,
  connects and sends by its path below the root, or below the source of
  the mount that holds it, from that directory as the calling thread's
  working directory (`pthread_fchdir_np`); a receiver reads such a
  datagram's relative source as the container path below the mount or the
  root where a socket of that path is. The library resolves names in `getaddrinfo` and `gethostbyname`
  as a resolver does: from the container's `/etc/hosts` first, which the
  host's resolver never reads, then from those names, and then as the host
  does. Network.framework resolves through libsystem_dnssd's private
  client, `dnssd_getaddrinfo`, which asks mDNSResponder over XPC, out of a
  container's reach, and reads each address from a result object: the
  library takes the lookup's name, queue and handler, never activates it,
  resolves the name as `getaddrinfo` does in the container, and hands the
  handler results of its own, which its getters answer for. Network.framework
  then connects with `connectx`, which the library routes as `connect`,
  unbinding a socket from the interface Network.framework bound it to
  where its connection goes to loopback instead. A datagram or connection to port 53 of a server outside the
  machine, IPv4 or IPv6, a query a program sends itself, goes to the
  container's router instead, which answers DNS on UDP at its own port
  and over TCP through it (a `DNSStream` request): the A and AAAA questions for names
  the stack answers, as the library resolves them, by itself, and any
  other query passed on to the servers of the container's `resolv.conf`,
  given internet access, refused without; the library presents the
  answer as from the server asked. It connects to a member's address through the host port
  behind its port, refuses a port the member does not expose, and reports
  a peer's host port as its member address and port. A TCP listener on a
  container port that has no host port becomes a Unix socket, in the same
  descriptor, in the container's port directory under the engine's state,
  which the container's profile lets it bind and reach and no other
  container's does; it reads as the TCP socket the program made. A bind
  to port 0, which the kernel would answer from its ephemeral range and
  Seatbelt refuses, takes a port of that range, 49152 to 65535, that no
  host port backs and no socket file in the port directory holds, the
  same way. Each
  container's shim runs its router, on a port of the stack's block that
  the network file names: a client sends a byte naming its request's kind
  and the container port it wants, two bytes, and the router joins the connection to that port's socket or
  refuses it, writing the client's loopback address and port there first,
  which the library takes on `accept` and reports as the peer, as a TCP
  server sees its client; a connection that carries none, as the library's
  own probes of a socket do, the library closes and accepts the next. The library connects a client of a member's port without a
  host port through the member's router, and one of its own container's
  through its own, at once, whether or not the socket blocks, and reports
  the peer as the member's address and port. A UDP socket on such a port
  becomes a Unix datagram socket there too. For each such port the router
  binds a flow port, a UDP port of the stack's block, and relays datagrams
  between it and the socket, with a six-byte header of the IPv4 source or
  destination on the Unix side alone. The library adds and strips that
  header in every call that sends or receives on the socket, and keeps a
  connected one's peer itself, as the router finds it: datagrams sent
  without a destination go there, and datagrams from anywhere else are
  dropped, as the kernel drops them for a connected UDP socket. It keeps
  the socket's requests for a datagram's destination too (`IP_RECVDSTADDR`,
  `IP_PKTINFO`, `IPV6_RECVPKTINFO`), which a Unix socket has no IP options
  for, and writes their control messages to `recvmsg` itself: the address
  the socket is bound to, or for one bound to the wildcard, its member
  address for a member's datagram and loopback for its own container's,
  on `lo0`. The library also copies a file to a stream for `sendfile`,
  which the kernel refuses with `EPERM` under a container's profile and
  Java's `FileChannel.transferTo` calls to answer a Kafka fetch, and it names
  the container's `/tmp` for `confstr`'s per-user directories, which Rust's
  `std::env::temp_dir` asks for when `TMPDIR` is unset. A client asks the router for the flow
  port once (a request for port 0, then the UDP port) and the library
  caches it, sends the client's datagrams to it as to a host port, and
  reports datagrams from it as the member's address and port. A container
  with network mode `container:<id>` joins that container's stack and takes
  its network wholesale: its ports and host ports, member address, network
  file, port directory and router, and its stack's egress proxy, in the
  container library's variables and the Seatbelt profile alike, so its
  processes bind and reach what that container's do. The stack's egress
  proxy reads the same file and reaches a member as the library does, for
  the container the client's credentials name, so a client that sends even
  its stack's traffic through `HTTP_PROXY`, as Python's `urllib` does for
  a name `NO_PROXY` leaves out, reaches the members that container sees,
  and the aliases its own links give them.
- **Capabilities.** A container runs as the engine's unprivileged user, so
  Docker's default capabilities grant it nothing to give up: `--cap-drop` is
  accepted, and `--cap-add` of anything beyond the default set, which would
  need root, is refused, but `IPC_LOCK`, which grants what macOS lets every
  process do already: macOS does not enforce `RLIMIT_MEMLOCK`, so any
  process may lock memory within the system's wired-memory limits. For the
  same reason `--ulimit memlock` is accepted only as unlimited.
  `no-new-privileges` holds for every container.

---

## 4. Image and root filesystem

### Layout

All state lives under one root, `~/.fws` by default (`fwsd -root`). The root
is resolved through symlinks at start-up, because Seatbelt matches resolved
paths.

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
  images/roots/<config-digest>/    # each image, unpacked once
  stacks/<stack>/network           # the stack's network file (FWS_NETWORK)
  stacks/<stack>/c/<id12>/
    r/                             # the container root: an APFS clone
    init/                          # the root as create left it, for diff
    config.json  exit.json  log.jsonl
    profile.sb  profile.bin        # the profile's text and its bytecode
    pids.max  pidns  shm           # the pids limit, a shared PID
                                   #   namespace's file, the shm ledger
    engine/                        # a privileged container's nested engine
  volumes/<name>/_data/            # named and anonymous volumes
  staging/  trash/                 # roots being cloned or deleted
  buildcache/                      # the classic builder's step cache
  buildkit/                        # BuildKit's content store, snapshots, metadata
```

A container's root is a clone of its image's, which takes as long to make
and to delete as the image has files. So the engine clones into `staging/`
and deletes from `trash/` without holding its lock, and moves a root into or
out of place under the lock with a `rename`. At start-up it deletes whatever
an earlier engine left in either, in the background.

Every image is an OCI image in `images/oci`, an OCI image layout whose
`index.json` names each tagged image as containerd and `docker save` name
them (`io.containerd.image.name`, `org.opencontainers.image.ref.name`). Pulled
and loaded images keep their manifests, configs and layers byte for byte, so
their digests and IDs (the config digest, as in Docker) survive a save, load,
push or pull. Removing an image removes the blobs no other image refers to.

The store can also stand on a directory of image archives (`fwsd
-image-archives`, `*.tar` as `docker save` and the catalog's build write
them; `internal/image/archives.go`). It reads each archive's names when it
opens, and loads an archive, once, the first time one of its names is asked
for and the store holds no image of that name. A catalog then costs only the
images a session uses, and the engine works offline from it; an image
removed after its archive loaded stays removed.

Registry credentials come from the client, as dockerd takes them: `docker
login` sends them to `POST /auth`, which checks them against the registry,
through its token exchange when it has one, before the CLI stores them; the
CLI then sends them with every pull and push in `X-Registry-Auth`, and with a
classic build, for every registry, in `X-Registry-Config`. The engine keeps
none of its own. A push reports each layer as dockerd does, `Layer already
exists` for one the repository holds and `Pushed` for one it uploads.

Besides tags, an image carries repository@digest references (`RepoDigests`):
the digest a pull resolved, which is the index's when a multi-platform index
was pulled, and the manifest digest a push wrote. Any reference, tag or
digest, names the image for `run`, `inspect`, `tag` and `rmi`. `rmi` follows
dockerd: it drops a reference while others remain, and a repository's digest
references go with its last tag. A pull resolves the reference first and
reports `Image is up to date` when it names the image already; otherwise it
downloads only the layers the store lacks, reporting the rest as `Already
exists`, as dockerd's pull stream does.

Each layer is unpacked once, by its diff ID, into `images/layers/<diffid>`,
holding that layer's entries alone with its whiteouts as the files that
name them, and checked against the config's `rootfs.diff_ids` as it is. An
image's root, `images/roots/<digest>`, is its layers laid over an empty tree
in order: a directory the tree lacks comes as one `clonefile` of the layer's
subtree, so images that share layers share their blocks. A layer with a hard
link into a lower layer is applied in place instead. Layers go with the
blobs once no image names their diff ID. BuildKit's snapshotter keeps the
layers it unpacks the same way (§8). On create, the engine gives the container an
**APFS clone** of it (`clonefile(2)`): copy-on-write, constant time, and near
zero bytes until the container writes. `clonefile` is the one primitive macOS
offers that fits this exactly: instant writable roots with no union mount and
no root.

The engine runs unprivileged, so layer extraction keeps each entry's
modification time and records what it cannot set: an owner other than root
in `com.fws.owner`, and setuid, setgid and sticky bits in `com.fws.mode`,
which the container library presents and nothing enforces. It skips device
nodes (the host's `/dev` serves containers) and refuses entries that would
land outside the root. The layers the engine writes — `docker commit`,
BuildKit's, `export` and `cp` — carry the recorded mode and owner, root
where none is recorded, never the host's user. `docker load` verifies each
blob against its digest. A pull downloads three layers at a time; a push
checks each blob once, mounts those another repository of the registry
holds, and puts the manifest last. The store sweeps what a crash left as it
opens.

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
copy-up does. A `noexec` tmpfs mount's directory is left out of the
profile's `process-exec` allowance, and Docker's `ReadonlyPaths` and
`MaskedPaths` become denials of writes, and of reads and writes, at the end
of the profile, at each path's place in the root or the mount holding it.

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
it: it assigns each container port a real host port, the container library
puts the assignment in the program's place, and the environment names it for
a program that loads no library.

- **Binding.** A service listens on its container port, as under Docker,
  and the container library binds that port's host port on loopback in its
  place (§3). Seatbelt allows binding exactly those host ports on loopback
  and nothing else. `SANDBOX_PORT_<n>` names the host port of port `<n>`,
  for a program that loads no library.
- **Discovery.** Each stack peer's ports arrive as
  `SANDBOX_<SERVICE>_PORT_<n>` with `SANDBOX_<SERVICE>_HOST=127.0.0.1`, for
  both the service name and the container name. With replicas, the names
  point at replica 1. A client of `db:5432` reads
  `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
- **Published ports.** `ports:` entries bind real host listeners in the
  container's shim (`internal/portproxy`), outside the sandbox, which
  forward each connection to the container's host port, or to the Unix
  socket in its port directory for a port without one. A
  taken host port fails the start with Docker's "port is already allocated",
  and a host port range binds the first port of it the host has free. UDP
  ports publish through a userland proxy, as Docker's `docker-proxy` does:
  one upstream socket per client, so replies reach the client that asked,
  and a flow ends after 90 seconds without traffic. A container may send
  only to its stack's ports, so each upstream socket binds a port of the
  stack's block, leased from the allocator for the flow's life. A container
  port takes one host port for TCP and UDP alike, which `SANDBOX_PORT_<n>`
  names.
- **Isolation.** A container may connect to every port of its own stack's
  block and nowhere else, so stack A cannot reach stack B even though both
  live on `127.0.0.1`. That includes B's published ports.
- **Shared networks.** Stacks whose containers share a user-defined network
  keep their blocks and profiles; each container's router, which runs in
  its shim outside the sandbox, carries its connections to the other
  stack's members. Member addresses are unique across stacks, and each
  stack's network file also lists the other stacks' members on a network
  one of its members is on, marked as foreign, with every member's
  networks. The library resolves and reaches only the members its
  container shares a network with or links to, and connects to a foreign one through
  its own router with a relay request of the member's address and port.
  The request carries the router's relay key, which the router keeps in
  the container's port directory, where no other member's sandbox reads,
  so a member reaches the router's relays only as its own container.
  The router reads the network file, checks that the member shares a
  network with its container or is linked to from it, and joins the connection to the member's
  host port, or to its router for a port without one. A UDP lookup of a
  foreign member's port at the container's own router gets a relay port
  of its own stack's block, which the library caches as a flow port and
  reports replies from as the member's address and port. The router
  sends each socket's datagrams on from its own socket, leased from the
  member's stack's block so the member may answer, to the member's host
  port or flow port, as a published UDP port's proxy does. A relay port
  lies in the stack's block, where every member may send; the router takes
  a datagram only from its container's own ports, its UDP host ports and
  flow ports, the ports its sockets send from. A UDP socket no program
  bound, which the kernel would bind to an ephemeral port on its first
  datagram, the library binds to an ephemeral port of the container's own
  before its first datagram to a foreign member, so the relay takes it.
- **Egress.** Seatbelt can allow loopback ports or every destination, nothing
  in between, so containers reach the internet through a proxy the engine
  runs for each stack on a port of the stack's block (`internal/egress`): an
  HTTP proxy, and on the same port a SOCKS5 one (RFC 1928, CONNECT only) for
  any TCP, told apart by the first byte a client sends. A container's proxy
  URLs carry its member address as the user and, as the password, an
  HMAC-SHA256 of the address under a key the engine draws for the stack and
  keeps in its state, which no container reads; the proxy checks them
  (`Proxy-Authorization: Basic`, or RFC 1929 for SOCKS5) and answers a
  wrong pair with 407, or SOCKS5's failure status. They name the member
  whose view of the stack the proxy resolves members in; a client with no
  credentials, a build step's, reaches no member. An address of a
  network's subnet that no member the client sees holds is unreachable
  through the proxy, as it is through the library.
  Containers, classic `RUN` steps and BuildKit steps receive `HTTP_PROXY`,
  `HTTPS_PROXY`, `ALL_PROXY` (`socks5h://`, so the proxy resolves names) and
  `NO_PROXY`, which leaves loopback, the unspecified addresses, which a
  connection reaches as loopback, and the member block to the container
  library. The proxy resolves each destination once, refuses it if any
  address is loopback, link-local, multicast, unspecified or assigned to one
  of the host's interfaces, and dials an address it checked, so DNS cannot
  rebind a name onto the host. Containers with `network_mode: none`, or only
  on `internal: true` networks, get no proxy variables and their profiles omit
  the proxy's port.
- **Networks.** User-defined networks decide which members a container
  resolves and reaches: the container library and the routers answer only
  for members it shares a network with or links to, and a network shared
  with another stack joins the two stacks through their routers (above).
  Seatbelt's fence is the stack's block, so between two members of one
  stack the separation is the library's, not the kernel's. Network mode
  `container:<id>` takes that container's network wholesale (§3a);
  `host` is refused, but for a nested engine's containers, which take the
  privileged container's network (§3a). Only the `bridge` driver and the
  default IPAM driver are accepted.
- **ICMP.** A container's traffic is held to its stack's TCP and UDP ports,
  and ICMP has none. The container library makes an unprivileged datagram
  ICMP socket, as `ping` opens, a Unix datagram socket pair, and answers an
  echo request to loopback, the container's own address or a member it
  sees at once, as that member's kernel would. It sends one to a host
  outside the machine, over IPv4, to the container's router as an `Echo`
  request (the address, the message's length, the message); the router,
  outside the sandbox, pings the host through macOS's unprivileged ICMP
  socket for a container with internet access, and answers with the reply,
  or refuses after 2 s. An echo to any other address fails with
  `ENETUNREACH`.

A program that loads the container library listens on and connects to
`5432` as written; one that loads none, as Apple's do not, needs the
`SANDBOX_*` variables.

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
  self-contained wherever its root lands. It writes an OCI image with a
  layer per store path, in sorted order, and a last layer with everything
  else, so images that share store paths share layers. The profile denies the host's
  `/nix`, so the tests prove it.
- **Entrypoints.** Where the official image's entrypoint configures the
  service, the catalog's is a Go program (`images/*/entry`) that does the
  same, on its container ports, and `exec`s it; other images run their
  program directly, or the upstream image's own `#!` scripts (Kafka's
  `configure`, the Lambda runtimes' `bootstrap`), which run under the
  interpreter in the root, and a `#!/usr/bin/env` line under the program
  the container's `PATH` names (`internal/runtime/script.go`).
- **Catalog.** `scripts/build-images.sh` builds every image under
  `images/`, among them `fws/postgres`, with `initdb`,
  `POSTGRES_PASSWORD`, `POSTGRES_DB` and `docker-entrypoint-initdb.d/*.sql`
  handled the way the official image handles them; `fws/redis`;
  `fws/toolbox`, a small HTTP app backed by its stack's Redis that doubles
  as the tests' probe; `fws/ryuk`, Testcontainers' reaper built from its
  release unchanged; databases, brokers and servers (MySQL, MariaDB,
  MongoDB, Kafka, RabbitMQ, NATS, nginx, Vault, SeaweedFS as the S3
  server and others); language runtimes (Node, Bun, Python, the Lambda
  runtimes); `fws/dind`; and the Kubernetes images (`kind-node`, the
  control plane's components, `kubelet`, `kubectl`, `pause`). An `nc`
  checks a container port through the container library, as
  Testcontainers' in-container port check runs `nc -z`. A container reaches
  a Unix socket it bind-mounts, as in Docker, and makes sockets in its
  writable mounts.
  macOS allows only 87 System V semaphores machine-wide by default, so the
  Postgres entrypoint keeps each server's process limits small.
- **Distribution.** Archives for `docker load`, or any registry as an
  ordinary image with a `darwin/<arch>` platform:
  `scripts/publish-images.sh` pushes each archive's OCI layout as
  `<prefix>/fws-<name>`.
  CI publishes the catalog to a local registry on every pull request to prove
  the script, and the end-to-end suite pulls through the engine from an
  in-process registry. Compose files name these
  images explicitly. The engine never substitutes an image name.

---

## 8. Docker Engine API surface

Served on `unix://~/.fws/docker.sock` at API version 1.51 (minimum 1.24),
with or without the `/v1.xx` prefix:

- `GET|HEAD /_ping`, `GET /version`, `GET /info`, `GET /events`,
  `GET /system/df` (in API 1.51's shape: image sizes counting each layer
  once, containers' writes, volume usage, and both builders' caches)
- Containers: `create`, `json` (list, with Docker's filters, and sizes with
  `size`), `{id}/json`, `{id}/changes` (`docker diff`),
  `start`, `stop`, `restart`, `kill`, `wait` (all three conditions),
  `rename`, `resize`, `logs` (follow, tail, since, until, timestamps),
  `attach` (hijacked, multiplexed or raw) and `attach/ws` (a websocket of
  unframed output, binary from API 1.28 on), both ending on a terminal at
  the client's detach keys (ctrl-p ctrl-q unless it names its own), `archive` (`HEAD`, `GET`, `PUT`:
  `docker cp`), `top`, `stats` (streamed every second, or one sample),
  `pause`, `unpause`, `update` (restart policy and resource limits),
  `DELETE`, `prune`
  (`label`, `until`)
- Exec: `POST /containers/{id}/exec`, `POST /exec/{id}/start` (attached,
  detached, TTY, detaching at the exec's detach keys), `/exec/{id}/resize`, `GET /exec/{id}/json`
- Images: `json`, `create` (registry pull for `darwin/<arch>` with
  `X-Registry-Auth`, or `docker import` with `fromSrc` `-` or an http(s)
  URL, `changes` and `message`), `load`, `{ref}/json`, `{ref}/tag`,
  `{ref}/history`, `get` and `{ref}/get` (`docker save`), `{ref}/push`
  (with `X-Registry-Auth`), `prune` (`dangling`, `until`, `label`),
  `DELETE`; references may be repository@digest
- Registries: `POST /auth` (`docker login`), `GET /distribution/{name}/json`
  (the descriptor and platforms a name resolves to in its registry, with
  `X-Registry-Auth`), `GET /images/search` (`docker search`, against Docker
  Hub's search API or the registry the term names)
- Containers to images: `POST /commit` (with `changes`, pausing by default)
  and `GET /containers/{id}/export`
- Networks: `json`, `create`, `{id}`, `connect`, `disconnect`, `prune`, `DELETE`
- Volumes: `json`, `create`, `{name}`, `prune`, `DELETE`
- The engine's own: `GET` and `PUT /fws/networks/{name}/services`, a
  network's virtual services, which `fws-kube-proxy` keeps in step with a
  cluster's (see Kubernetes below)

A container's create ends with an APFS clone of its root, taken once its
mounts are set up: the state Docker's init layer holds. `docker diff` and a
container's size compare the root against it, so they report what the
container wrote and nothing the engine set up.

Health checks run on the image's or container's own cadence (interval,
timeout, start period, retries) and report Docker's states and events. Restart
policies follow Docker's back-off.

Requests the engine cannot honour return an error rather than being
accepted, `501` for a knob only a kernel or root could enforce: a swap limit
apart from the memory limit, memory reservations, swappiness and disabling
the OOM killer; cpusets, real-time CPU scheduling and per-device block I/O
limits; the `nproc`, `data`, `as`, `rss` and similar ulimits, and a finite
`memlock`; devices, cgroups, sysctls, user namespaces and storage
options; a capability beyond Docker's default set but `IPC_LOCK`,
and security options; `host` networking outside a nested engine; non-`bridge`
network drivers; IPAM drivers other than the default; and SCTP publishing.
Memory, CPU, pids and ulimit limits
otherwise hold, and `docker update` changes them live (§3a); privileged
mode runs a nested engine (§3a); a non-default user is presented by the
container library (§3).

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
- **Images.** `FROM` resolves the engine's images first: on first use an
  image's manifest, config and layers are written into BuildKit's content
  store as stored, so BuildKit unpacks and caches it layer by layer. The
  `moby` exporter, which `docker build` and `--load` use, writes the result
  with BuildKit's own image writer as an OCI image whose layers extend the
  base's, stores it in the engine's image store, and clones the result's
  snapshot as the image's root.

The BuildKit packages the engine uses live in `third_party/buildkit`, copied
by `scripts/sync-buildkit.sh` with their imports rewritten into this module
and the patches in `third_party/buildkit-patches` applied:

- `localmounter_darwin`: BuildKit's darwin mounter mounts a read-only
  snapshot with `mount(2)`, which needs root; the patched mounter reads the
  snapshot's directory directly, as it already did for read-write ones.
- `ssh_socket_owner`: a step's SSH agent socket stays the worker's user's,
  which stands for the step's root, since no other owner can be set
  without root.
- `syntax_builtin`: a `# syntax=` directive naming Docker's own Dockerfile
  frontend, a Linux image a darwin worker cannot run, is served by the
  built-in frontend instead of being forwarded.
- `resolvecache_race`: the frontend's resolve cache is set up once, before
  stages resolve their bases concurrently.

Keeping the packages in the module, rather
than behind a `replace` directive, keeps `go install …/cmd/fwsd@<version>`
working.

`POST /build` is the classic builder (`internal/dockerfile` parses; the
engine builds), for `DOCKER_BUILDKIT=0`. Each stage starts from an APFS clone of its base image, or an
empty root for `scratch`. `COPY` and `ADD` resolve paths inside the stage
root as a chroot would, absolute symlinks from the root, so nothing lands
outside it; they read the uploaded context, which the CLI has already filtered
through `.dockerignore`, or an earlier stage or image with `--from`. `RUN`
executes under a Seatbelt profile confined to the stage root, with internet
only through an egress proxy on a stack the build owns. Each `RUN`,
`COPY` and `ADD`, and a `WORKDIR` that makes its directory, adds a layer: the
difference, whiteouts included, between an APFS clone of the stage root taken
before the step and the root after it. The image is the base's layers and the
last stage's, as an OCI image, and the last stage's root moves into the image
store as its root. `docker commit` adds one layer the same way, the
container's root against its image's.

The classic builder caches each step as dockerd's reuses intermediate
images. A key chains every instruction from the base image's ID; a `RUN`
also folds in the build arguments in effect, and a `COPY`, `ADD` or
directory-making `WORKDIR`, which are cheap, runs and folds in what it
changed, by entry names, modes, links and contents, times left out. A `RUN`
found in the cache does not run: its layer goes on the root. A cheap step
found in the cache gives way to the cached layer, byte for byte, and the
cache keeps each step's first history time, so a rebuild that hits on every
step makes the same image. `--no-cache` skips lookups; `docker image prune`
and `POST /build/prune` empty the cache, as pruning dangling intermediate
images does in Docker. `ONBUILD` records triggers in the image config, and a
stage built on such an image runs them first. With `remote`, the engine
fetches the context itself: a Git repository at the fragment's ref and
subdirectory, fetched shallow with submodules, a tarball, or a plain
Dockerfile.

`pause` has the container's shim send SIGSTOP to every process it tracks
(§3a), so its CPU throttling never continues them, and `unpause` SIGCONT;
health checks and `exec` wait while a container is paused, and stopping a
paused container resumes it to deliver the stop signal.

`top` reads the host's process table through `ps`, as dockerd's `top` does,
and keeps the container's processes: those of its main process's and its
running execs' process groups, those carrying its `SANDBOX_ROOT` tag, and
those macOS holds its main process responsible for, but the exec anchor.
`stats` sums the same processes' CPU time; `system_cpu_usage` is the
machine's CPU time elapsed since the engine started, so the client's CPU
percentage is the share of one CPU the container used. Memory (the physical
footprint the memory limit holds), CPU throttling, block I/O and network
counts come from the shim. The pids count is the number of those
processes, while the pids limit counts the shim's members (§3a); the two
can differ for a process the shim has not yet followed.

`docker cp` resolves paths as the container's processes do, absolute symlinks
on the host, and refuses any result outside the container's root and mounts.
It archives mounts as their contents rather than as the symlinks that
implement them.

---

### Podman's API

The socket serves Podman's API beside Docker's, under `/libpod`, versioned
by Podman's releases (`/v5.8.0/libpod/...`), with Podman's
`Libpod-API-Version` header (`internal/api/libpod*.go`). The endpoints that
speak Docker's protocol (attach, exec, kill, pause, rename, resize,
archive) pass to Docker's handlers; the rest take and give Podman's
shapes: a SpecGenerator becomes the Docker create request that does what
it asks (Podman's client names signals by their Linux numbers), list,
inspect, remove and wait answer as Podman's do (wait also for running,
healthy and unhealthy, on the engine's events), logs come framed by
stream even from a terminal, and pulls stream Podman's progress lines.
Pods are the engine's: a record per pod, kept in `pods.json`, and Podman's
`io.podman.pod` label on each member. Errors carry Podman's `cause` and
`response` beside Docker's `message`, for both clients.

### Kubernetes

- **CRI.** The socket also serves the Kubernetes Container Runtime
  Interface (`internal/cri`): a request over unencrypted HTTP/2 with a
  gRPC content type goes to it, the rest to the Docker and Podman
  handlers. It maps CRI onto the engine as cri-dockerd maps it onto
  Docker. A pod sandbox is a pause container (`fws/pause:3.10.1`) that
  holds the pod's network, ports and DNS, and the pod's containers join it
  as `--network container:` joins one. Every pod not on its node's network
  joins the user-defined network `fws-pods`, whose subnets are the pod
  CIDR the kubelet sends, IPv4, IPv6 or one of each, IPv6-only for an
  IPv6 one alone, or kind's `10.244.0.0/16` until it sends one. The
  engine copies each container's output to the log file the kubelet names,
  in CRI's format, and serves exec, attach and port-forward streams on a
  loopback port, or a nested engine's on its privileged container's.
- **Nodes.** A kind node is a privileged container whose nested engine
  (§3a) is its kubelet's runtime. The node keeps containerd's
  configuration, which kind patches: the CRI reads it at each pull and
  pulls through the registries its mirrors and hosts directory name,
  dialling them as the node reaches them, by their names on its networks
  (`internal/containerd`, `Engine.DialNode`), and reports the runtime not
  ready while it sets anything else.
- **Cross-node pods.** Nested engines in privileged containers that share
  a network are a cluster's nodes (`internal/engine/peers.go`). Each
  nested engine publishes its members on networks with subnets of their
  own to the engine it is nested in, which hands them to the nested engines
  of the containers its container shares a network with. Each lists them in
  its network files as foreign members of the networks of the same names,
  reached through their routers, which listen on the host's loopback as
  every router does, so a pod reaches another node's pods at their
  addresses, IPv4 and IPv6. A pod on an IPv6-only network has no IPv4
  address to list it at: a node lists it under a key of its own in the
  member block, which no member holds, by which the library routes its
  IPv6 address and the container's router finds it (`peerKeys`). The outer engine refreshes the others from a goroutine of its
  own, and takes no nested engine's lock while holding its own.
- **Services.** A network can carry virtual services
  (`internal/engine/services.go`), as Kubernetes Services' cluster IPs:
  each has an address on the network, names (`name`, `name.namespace`,
  `name.namespace.svc` and `name.namespace.svc.cluster.local`) and TCP or
  UDP ports, each with its endpoints' addresses and ports. The engine
  serves each service port on a loopback port, outside any sandbox, and
  carries each connection, or each UDP client's flow, to the next of its
  endpoints, as kube-proxy's userspace mode does. The network file lists
  services as members of the stack `services`, so the library resolves and
  reaches them as it does members. A TCP service port with a node port
  also answers on the node's port of that number: for a nested engine,
  its privileged container's port.
- **fws-kube-proxy.** `cmd/fws-kube-proxy` is the node's kube-proxy. It
  polls the API server for Services and EndpointSlices (every second by
  default, `-interval`), with no client library, and `PUT`s the whole list
  to `/fws/networks/fws-pods/services` on the engine's socket
  (`FWS_ENGINE`) when it changes, and again every 30 s (`-resync`), so an
  engine that restarted gets it back.

## 9. Runtime launch sequence

On create: clone the image root, assign the stack's ports, realise the
mounts, and record the container. On start:

1. Render the Seatbelt profile from the container's grants (§3), write it
   to `profile.sb` next to the container's state, outside the root, and
   compile it to `profile.bin` unless the bytecode of the same text is
   kept already (§3a), alongside step 3.
2. Build the environment: image and container variables, `PATH` mapped into
   the root, `HOME` under the root, and the `SANDBOX_*` variables. With
   `--init`, link the `fws-shim` binary at `/sbin/docker-init` in the root,
   as Docker mounts its init, and put it before the command: run by that
   name it acts as tini, forwarding the signals it receives to the
   command, reaping every process that ends up its child, and exiting as
   the command does.
3. Start the container's shim (§3a), which binds the published host ports.
4. The shim spawns the helper, which applies the limits and the Seatbelt
   profile and executes `<root>/<argv0> …` with the container library
   loaded (a script under its interpreter from the root), the main process
   leading its own process group (or session, with a TTY). Output reaches
   the json-file log through real pipes, so the shim sees the exit at once
   even if a descendant still holds stdout.
5. When the main process exits, the shim kills the rest of the container,
   drains the output for up to 2 s, records the exit, and tells the engine, which applies
   the restart policy.

`exec` and health checks start further processes in the container's
sandbox, through its anchor, with its root and environment, and with pipes
or a pty (`creack/pty`).

What the container library costs each process start, the time to reach
`main` (`BenchmarkStartCost`, CI's macOS 26 runner, medians of interleaved
rounds): about 3.4 ms natively, 7 ms with any library inserted, which
keeps the loader from its prebuilt launch, and about 4 ms more for the
library's interposing, some 25 µs for each of its 160 functions. The cost
does not depend on which functions: each eighth of them, the most widely
bound (`read`, `write`, `close`) or the rarest (`proc_*`), costs alike. A
start is cheaper only with fewer functions interposed, that is with fewer
of the container's features.

---

## 10. Go implementation

The engine is Go and cross-compiles with `CGO_ENABLED=0` for `darwin/arm64`,
Apple silicon, alone. What runs inside every container process start is C,
which the engine compiles with clang on the Mac before its first container
starts, into `lib/`: the container
library (`internal/pathmap/src/fwsroot.c`) and the spawn helper
(`internal/pathmap/src/spawn.c`). `clonefile` comes from
`golang.org/x/sys/unix`, image handling from `go-containerregistry`, and the
API types from `github.com/moby/moby/api`, so the JSON matches what Docker
clients decode.

```
cmd/fwsd/             # the daemon: Docker Engine, libpod and CRI APIs over a unix socket
cmd/fws-shim/         # the helper binary: supervisors, exec anchors, egress proxies, docker-init
cmd/fws-mkimage/      # Nix closure -> darwin image archive
cmd/fws-kube-proxy/   # kube-proxy for a node whose runtime is the engine
internal/alloc/       # per-stack port blocks and directories
internal/api/         # the Docker Engine API (the Compose subset) and Podman's libpod
internal/buildkit/    # embedded BuildKit with a darwin worker
internal/cri/         # the Kubernetes Container Runtime Interface
internal/dockerfile/  # Dockerfile parsing and variable expansion
internal/egress/      # the per-stack HTTP and SOCKS5 proxy to the internet
internal/engine/      # containers, volumes, networks, exec, health, events, machine guards
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
internal/router/      # traffic to container ports without host ports, relays, ICMP echo
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
macOS runner; no test skips for a missing tool. The unit tests run on Linux
where they are portable and on macOS in full, where the Seatbelt and
container-library packages run probes under the real sandbox. The
end-to-end suite drives the stock Docker CLI, Docker Compose, Podman,
Testcontainers, kind and `crictl` against an engine of its own. The
two-stack test that was the first milestone's definition of done — two
Compose stacks of `postgres`, `redis` and an app, unable to reach each
other's ports or paths, with `down` on one leaving the other working —
still runs in it. [`CONTRIBUTING.md`](../CONTRIBUTING.md) lists every check
CI runs and how to run each locally.

What the engine cannot do, and why, is in
[`LIMITATIONS.md`](../LIMITATIONS.md); how the project got here is in
[`HISTORY.md`](HISTORY.md).
