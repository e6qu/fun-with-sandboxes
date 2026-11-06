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
| Process signalling | Seatbelt `(deny signal)` and `(deny process-info*)` except same-sandbox; execs start in the container's sandbox; the container library lists the sandbox's processes alone | A cannot `kill`, inspect or list B's processes | pids are the host's (no PID namespace) |
| Default deny | Seatbelt `(deny default)` on Apple's `system.sb` baseline, each operation class re-allowed by name | A container gets no IOKit, Mach registration, preferences, AppleEvents, kernel control sockets or anything else the profile does not name | — |
| IPC | Seatbelt `ipc-posix-name-prefix` filters; the container library's per-container System V keys | POSIX shared memory and semaphores confined to a per-container name prefix (`--ipc` private); System V keys hashed with it; `none` denies all IPC | a System V key is presented, not confined: a guessed hashed key reaches another container's object |
| Resource limits | Per-container supervisor (§3a); `setrlimit` through a spawn helper | Memory, CPU, pids caps, `--ulimit`, priority and disk I/O tiers | Enforced by ending, stopping or killing processes, not by refusing allocations; the container library refuses forks at the pids limit |
| Identity | Runs as the invoking user | — | No `user:`-switch to arbitrary UID (needs root) → non-default `user:` is **rejected** |

Rejections are deliberate: an unsupported knob returns a clear API error rather
than silently doing nothing. Silent no-ops would be a fidelity bug.

### 3a. Isolation and limits without cgroups

macOS gives an unprivileged process no cgroups, no namespaces and no
capability sets. What it does give — `posix_spawn` attributes, `setrlimit`,
`setpriority`, `setiopolicy_np`, `proc_pid_rusage`, kqueue process events,
`SIGSTOP`/`SIGCONT`, and Seatbelt's filters — the engine composes into the
same knobs, each enforced by a real mechanism or refused:

- **Spawn helper.** Every container's main process and every build step
  starts as `fws-shim`, a small binary beside `fwsd` that holds
  only the engine's helper roles, in its spawn-helper role, through
  `posix_spawn` with responsibility disclaimed, so the process inherits no
  privacy (TCC) grant of whatever launched the engine. The helper applies
  `--ulimit` (nofile, fsize, cpu, core, stack), the nice value `--cpu-shares`
  maps to, the disk I/O policy tier `--blkio-weight` maps to, and the
  controlling terminal, applies the Seatbelt profile, and executes the
  command with the container library loaded. A limit the user cannot
  set fails the start, reported on a close-on-exec status pipe.
- **Exec anchor.** Seatbelt lets a process signal and inspect only the
  processes of its own sandbox, the ones that inherited it, and a process
  that enters the container's profile by itself is a sandbox of its own. So
  a main process's spawn helper, once sandboxed, starts the container's
  anchor before it executes the command:
  - The anchor is a process of `fws-shim` that a launcher starts and leaves
    at once, so it is no child of the main process, whose `wait` would
    otherwise wait for it.
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
- **Supervisor.** Each container runs under a shim, `fws-shim` in its
  supervisor role and a session of its own; each stack's egress proxy is
  `fws-shim` too. The shim owns the container's
  output and json-file log, its pty, its published-port proxies, and its exit
  status, and answers the engine over a Unix socket in `<root>/run`. It
  tracks the container's processes — the main process group, descendants
  found through kqueue `NOTE_FORK` events and the process table's parent
  links, and anything that started with the container's `SANDBOX_ROOT` tag —
  and enforces, every 100 ms:
  - `--memory`: the sum of the processes' physical footprints, which counts
    compressed and swapped-out memory, so `--memory-swap` may only equal it;
    past the limit the largest process is killed, as the kernel OOM killer
    picks one in a cgroup, and the container reports `OOMKilled`.
  - `--cpus` / `--cpu-quota`: the processes' CPU time against the quota; a
    run over it stops them with `SIGSTOP` until the quota has paid it back,
    and the throttled periods and time show in `docker stats`.
  - `--pids-limit`: the engine writes the limit to the container's
    `pids.max`, as a pids cgroup holds it, and rewrites it on
    `docker update`; the container library reads it before a `fork` or a
    `posix_spawn` that starts a process, and fails it with `EAGAIN` when
    the container already runs that many. Processes past the limit, which
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
- **Live restore.** The shim and each stack's egress proxy are processes of
  their own, so with `-live-restore` the engine stops without stopping its
  containers, and the next engine reconnects to the shims through their
  sockets. Without it, an engine stops its containers as it stops, and ends
  any an earlier, crashed engine left running as it starts.
- **Container library.** Every container process loads `fwsroot`
  (`internal/pathmap`), a library the engine builds from embedded C source
  and the spawn helper injects with `DYLD_INSERT_LIBRARIES`, having applied
  the Seatbelt profile itself through `sandbox_init_with_parameters` so no
  Apple binary sits between them to drop the variable. It interposes
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
  `setuid` and its kin switch the presented identity, and every program a
  process executes inherits the identity it holds then, with the
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
  wildcard or loopback address on a container port binds the host port on
  `127.0.0.1`, a `localhost` connection or datagram to a container port
  reaches its host port, and `getsockname`, `getpeername`, `accept` and
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
  as it is created and kept in its record. The engine keeps a network file
  per stack, rewritten as members come, go, are renamed or change networks,
  of each member's address, the names it answers to (its container name,
  service, hostname, DNS names and aliases) and its ports' host ports; the
  container reads it (`FWS_NETWORK`) and nothing else of the engine's
  state. The library presents the container's interfaces where a program
  lists them, in the routing sysctl (`NET_RT_IFLIST`, `NET_RT_IFLIST2`)
  that `getifaddrs` and Go read and in the `SIOCGIF*` ioctls: the host's
  loopback interface, and `eth0`, which takes the index after the host's
  highest, with the member address. A Unix socket's address maps as a
  file path does, in `bind`, `connect` and `sendto`, and reads back as the
  container's path. A host path past the address's 104 bytes binds and
  connects by its last component, relative to its directory, with
  `bindat` and `connectat`. The library resolves names in `getaddrinfo` and `gethostbyname`
  as a resolver does: from the container's `/etc/hosts` first, which the
  host's resolver never reads, then from those names, and then as the host
  does; it connects to a member's address through the host port
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
  the network file names: a client sends the container port it wants, two
  bytes, and the router joins the connection to that port's socket or
  refuses it. The library connects a client of a member's port without a
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
  on `lo0`. A client asks the router for the flow
  port once (a request for port 0, then the UDP port) and the library
  caches it, sends the client's datagrams to it as to a host port, and
  reports datagrams from it as the member's address and port. A container
  with network mode `container:<id>` joins that container's stack and takes
  its network wholesale: its ports and host ports, member address, network
  file, port directory and router, and its stack's egress proxy, in the
  container library's variables and the Seatbelt profile alike, so its
  processes bind and reach what that container's do. The stack's egress
  proxy reads the same file and reaches a member as the library does, so a
  client that sends even its stack's traffic through `HTTP_PROXY`, as
  Python's `urllib` does for a name `NO_PROXY` leaves out, reaches the
  member too.
- **Capabilities.** A container runs as the engine's unprivileged user, so
  Docker's default capabilities grant it nothing to give up: `--cap-drop` is
  accepted, and `--cap-add` of anything beyond the default set, which would
  need root, is refused. `no-new-privileges` holds for every container.

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
  images/images.json               # image records: tags, config, manifest
  images/oci/                      # an OCI image layout: oci-layout,
                                   #   index.json, blobs/sha256/
  images/roots/<config-digest>/    # each image, unpacked once
  stacks/<stack>/c/<id12>/
    r/                             # the container root: an APFS clone
    config.json  log.jsonl  profile.sb
  volumes/<name>/_data/            # named and anonymous volumes
  networks.json  volumes.json
```

Every image is an OCI image in `images/oci`, an OCI image layout whose
`index.json` names each tagged image as containerd and `docker save` name
them (`io.containerd.image.name`, `org.opencontainers.image.ref.name`). Pulled
and loaded images keep their manifests, configs and layers byte for byte, so
their digests and IDs (the config digest, as in Docker) survive a save, load,
push or pull. Removing an image removes the blobs no other image refers to.

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

An image is also unpacked once, all layers applied in order with OCI
whiteouts, into `images/roots/<digest>`. On create, the engine gives the container an
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
  The router reads the network file, checks that the member shares a
  network with its container or is linked to from it, and joins the connection to the member's
  host port, or to its router for a port without one. A UDP lookup of a
  foreign member's port at the container's own router gets a relay port
  of its own stack's block, which the library caches as a flow port and
  reports replies from as the member's address and port. The router
  sends each socket's datagrams on from its own socket, leased from the
  member's stack's block so the member may answer, to the member's host
  port or flow port, as a published UDP port's proxy does.
- **Egress.** Seatbelt can allow loopback ports or every destination, nothing
  in between, so containers reach the internet through a proxy the engine
  runs for each stack on a port of the stack's block (`internal/egress`): an
  HTTP proxy, and on the same port a SOCKS5 one (RFC 1928, CONNECT only, no
  authentication) for any TCP, told apart by the first byte a client sends.
  Containers, classic `RUN` steps and BuildKit steps receive `HTTP_PROXY`,
  `HTTPS_PROXY`, `ALL_PROXY` (`socks5h://`, so the proxy resolves names) and
  `NO_PROXY`, which leaves loopback and the member block to the container
  library. The proxy resolves each destination once, refuses it if any
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
  self-contained wherever its root lands. It writes an OCI image with a
  layer per store path, in sorted order, and a last layer with everything
  else, so images that share store paths share layers. The profile denies the host's
  `/nix`, so the tests prove it.
- **Entrypoints are Go programs** (`images/*/entry`) that read the
  `SANDBOX_*` variables, configure the service, and `exec` it. Images carry no
  `#!` scripts: the kernel resolves interpreters on the host.
- **Catalog.** `fws/postgres:16`, with `initdb`, `POSTGRES_PASSWORD`,
  `POSTGRES_DB` and `docker-entrypoint-initdb.d/*.sql` handled the way the
  official image handles them; `fws/redis:8`; and `fws/toolbox:1`, a small
  HTTP app backed by its stack's Redis that doubles as the tests' probe;
  and `fws/ryuk:0.14.0`, Testcontainers' reaper built from its release with
  a patch that makes it listen on loopback, an entrypoint that points it at
  its assigned port and at the engine socket Testcontainers mounts, and an
  `nc` that checks a container port through its assigned host port, as
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
  `pause`, `unpause`, `update` (restart policy), `DELETE`, `prune`
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

A container's create ends with an APFS clone of its root, taken once its
mounts are set up: the state Docker's init layer holds. `docker diff` and a
container's size compare the root against it, so they report what the
container wrote and nothing the engine set up.

Health checks run on the image's or container's own cadence (interval,
timeout, start period, retries) and report Docker's states and events. Restart
policies follow Docker's back-off.

Requests the engine cannot honour return an error rather than being
accepted: `501` for memory, CPU, PID, ulimit and device limits; privileged
mode; capabilities; non-default users; `host` networking; non-`bridge`
network drivers; custom IPAM; SCTP publishing; and resource updates.

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
3. Start the container's shim (§3a), which binds the published host ports.
4. The shim spawns the helper, which applies the limits and the Seatbelt
   profile and executes `<root>/<argv0> …` with the container library
   loaded (a script under its interpreter from the root), the main process
   leading its own process group (or session, with a TTY). Output reaches
   the json-file log through real pipes, so the shim sees the exit at once
   even if a descendant still holds stdout.
5. When the main process exits, the shim kills the rest of the container,
   drains the output, records the exit, and tells the engine, which applies
   the restart policy.

`exec` and health checks start further processes in the container's
sandbox, through its anchor, with its root and environment, and with pipes
or a pty (`creack/pty`).

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
internal/runtime/     # the spawn helper: limits, Seatbelt, the container library
internal/pathmap/     # the container library: paths, identity, users, hostname
internal/identity/    # resolve --user and USER against the container's files
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
- Images must be macOS builds; programs the container library cannot load
  into must follow the `$SANDBOX_ROOT` + configurable-port contract.
- Memory and CPU limits are enforced by a supervisor that kills or stops
  processes, not by a kernel that refuses an allocation; the container
  library refuses a fork at the pids limit, and the supervisor kills what
  gets past it.
- `user:` and `USER` are presented by the container library; every process
  runs as the invoking user.
- Pids are the host's but the main process's, which the container library
  presents as 1, and a program that loads no library lists every process
  of the user; listings through the library, and every action, stay
  within the sandbox.
- Seatbelt's `sandbox_init` and `sandbox-exec` are Apple-deprecated (since
  2016) but still functional and used by Chrome, Bazel, and the major
  coding-agent sandboxes; there is no supported replacement for this use.
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
