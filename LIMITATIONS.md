# Limitations

Every limit here follows from two constraints — no root and no virtual
machine — and from what macOS offers an unprivileged process under them.

## Images

- **macOS-native images only.** Nothing on macOS runs Linux ELF binaries
  without a VM. Stock Docker Hub images do not run.
- **Images follow the container contract.** A service finds its root and
  ports at their container paths and port numbers through the container
  library, or reads them from `SANDBOX_*` variables (see the README).
  Software the library cannot load into needs an entrypoint that configures
  it from the variables.
- **Catalog images are built by Nix**, not taken from the vendors'
  binaries. Where a build records itself, as MySQL's and MariaDB's
  compilation comment does, the catalog builds it as the vendor does.
- **A container's view is presented by a library, not the kernel.** Every
  container process loads the engine's container library, which maps
  absolute paths into the container's root (as fakechroot does), presents
  the container's user and group (as fakeroot does), its users and groups
  from its own `/etc/passwd` and `/etc/group`, and its hostname. So
  arguments, entrypoints, `CMD`, `--user`, `USER`, `exec -u`, `--hostname`
  and `-w` behave as in Docker. Its limits:
  - A path names the root's file when the root holds it, or its parent, so
    a file can be made there; any other path, such as `/dev` or the system
    frameworks, names the host's. Symbolic links resolve inside the
    container, as under chroot: an absolute target starts at the
    container's `/`, and `..` stops there.
  - Programs Apple signs, and programs with the hardened runtime, load no
    library: the host's `/bin/sh`, `/usr/bin/env` and Chrome see host paths.
    Catalog images carry their own `/bin/sh`, so `system()`, `popen()` and
    package managers keep the library, and a `/bin/nc` for the `nc -z`
    port check Testcontainers runs inside a container, as macOS has no
    `/proc/net/tcp` for it to read.
  - A statically linked program, or one making system calls without
    libSystem, sees host paths. Go and every other macOS program use
    libSystem.
  - The identity is presented, not enforced: every process runs as the
    engine's user, so a container's non-root user can do what its root can,
    and `chown` records an owner in an extended attribute that `stat`
    reports rather than changing the file's owner.
  - A script's `#!` interpreter runs from the root when the root holds it,
    else from the host.
- The engine builds the library with clang on the first container start:
  install the Xcode Command Line Tools (`xcode-select --install`).
- **BuildKit builds darwin images on one worker.** `docker build`,
  `docker buildx build` (docker driver) and `docker compose build` run the
  real BuildKit solver with its cache, secret and SSH mounts included. Not
  supported: multi-platform builds and `# syntax=` directives (gateway
  frontends ship as Linux images). A step runs as the Dockerfile's `USER`
  through the container library, but an SSH mount's socket belongs to the
  engine's user whatever `uid` it names.
- **The classic builder (`DOCKER_BUILDKIT=0`) takes no heredocs**, as
  Docker's classic builder takes none; BuildKit builds take them.
- **Build steps run under Seatbelt** with internet through a build-only
  egress proxy (none with `--network none`); shell-form `RUN` needs
  `/bin/sh` in the image, as in Docker.
- **`docker commit` and `docker export` leave volumes out**, as Docker does:
  a commit keeps the image's own content at a mount point, an export an
  empty directory.
- **Images keep one platform.** Pulling or loading a multi-platform index
  stores its `darwin/<arch>` image alone, and a BuildKit build's
  attestations stay out of the engine's store, as dockerd's classic image
  store leaves them out.
- **The engine's image store holds no images from an earlier, flattened
  store.** `fwsd` refuses to open such a store; remove its `images`
  directory and load or pull the images again.

## Isolation

- **Not a security boundary against hostile code.** Containers run as the
  engine's user. Seatbelt confines them well enough to keep stacks apart and
  the host's data out of reach, but it is a development and test tool.
- **Processes are presented, with host pids but pid 1's.** There is no
  unprivileged PID namespace. Through the container library:
  - a container's process listings (`sysctl`'s process table, libproc's
    pid lists) hold its own processes alone, its execs among them;
  - its main process is pid 1, to `getpid`, `getppid`, `kill`, process
    groups and sessions, `proc_pidinfo` and the process table, so
    `docker exec x kill -HUP 1` reaches it;
  - and Seatbelt denies signalling or inspecting any other process.

  Every other process keeps its host pid, and a program that loads no
  library, as Apple's `ps` does not, sees host pids and lists every
  process of the user.
- **System V IPC keys are presented, not confined.** POSIX shared memory
  and semaphore names are confined to a per-container prefix
  (`SANDBOX_IPC_PREFIX`) under the default private IPC mode, and
  `--ipc none` denies all IPC. Seatbelt cannot tell System V keys apart, so
  the container library gives each container a key space of its own: it
  hashes each key with the container's prefix, and two containers using
  one key, as two PostgreSQL servers on port 5432 do, get objects of their
  own. A program that loads no library, or one that guesses another
  container's hashed key, can still reach that container's objects.
- **POSIX shared memory and semaphore names carry the prefix.** Under
  private IPC the container library gives every name the program uses
  `SANDBOX_IPC_PREFIX`, and a name too long to carry it within macOS's
  31 bytes becomes the prefix and a hash of the name. A program that loads
  no library, as Apple's do not, must add the prefix itself;
  `--ipc host` lifts the confinement.
- **Namespace modes are presented where the library can present them.**
  `--uts host` leaves the hostname unpresented, so the container reads the
  host's. `--ipc` modes confine IPC names (above). `--network container:`
  (Compose's `network_mode: service:`) gives a container the other's ports,
  address, hostname and stack, so its listeners answer on the other's
  ports. When that container stops, so does the router behind its ports
  without host ports, and the joining container's listeners there go with
  it. `--pid` and `--userns` need kernel namespaces and are refused.
- **Capabilities grant nothing to drop.** Containers run as the engine's
  unprivileged user, so `--cap-drop` changes nothing; `--cap-add` beyond
  Docker's default set needs root and is refused. AppArmor, SELinux and
  seccomp options other than `unconfined`/`disable` are refused.
- **The macOS system tree is readable** (`/System`, `/usr/lib`, `/usr/share`,
  `/bin/sh` and friends): binaries cannot run without it, and it holds no
  user data.
- **The host's system shell is executable**, because `popen()` and
  `system()` hard-code `/bin/sh`.
- **A container's processes are those that started with its root in their
  environment.** A process that leaves the container's process group, or its
  session, keeps `SANDBOX_ROOT` in the environment it started with, so a
  stop still kills it and `top` and `stats` still count it; only one that
  executes a program with that variable cleared escapes the container.

## Networking

- **ICMP reaches nothing.** A container's traffic is held to its stack's TCP
  and UDP ports; `ping` opens its socket but sends nowhere.
- **Container addresses are presented, not routed.** Each member of a stack
  has an address in `127.88.0.0/16` that the container library resolves its
  names to and reaches its ports through, so a client connects to `db:5432`
  as on a Compose network. The kernel never sees those addresses:
  - A program that loads no library resolves no member name; it uses
    `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
  - A server sees its clients connect from `127.0.0.1`, not from their
    member addresses.
  - A member's port without a host port is reached through its router,
    whose connections and datagrams its server sees come from `127.0.0.1`.
- **A container's interfaces are presented.** `getifaddrs`, `if_nameindex`,
  the `SIOCGIF*` ioctls and the routing sysctl Go's `net.Interfaces` reads
  list the loopback interface and, on a network, `eth0` with the member
  address in `127.88.0.0/16` and a MAC address of Docker's form, where the
  host's interfaces are. The kernel knows no `eth0`: binding a socket to
  its index, or joining a multicast group on it, fails.
  The member address is a loopback address, so a program that advertises
  its first non-loopback address finds none: SeaweedFS advertises
  `localhost`, where on Docker it advertises the container's address.
- **A stack's block is one reachability domain.** Seatbelt lets a container
  reach every port in its stack's block and nothing else, so within a stack
  network membership is presented, not enforced: the container library
  resolves and connects to the members a container shares a network with,
  and a program that loads no library reaches any member's host port.
- **Stacks meet on a network they share through their routers.** Containers
  of different stacks on one user-defined network, such as two Compose
  projects on an external network or two `docker run` containers on a
  `docker network create` one, reach one another by name: a container's
  router, outside its sandbox, relays its connections to the other stack's
  members. Its limits:
  - A datagram crosses through a relay port of the sender's router, which
    keeps one upstream socket per sending socket while it is used, as a
    published UDP port does, so a relay's concurrent senders are bounded
    by the other stack's block. A relay outlives a disconnect for the
    senders already using it, until 90 seconds pass without traffic.
  - A program that loads no library cannot reach the other stack's members,
    and its stack's egress proxy reaches them for any member of its stack.
  - A relay's UDP port lies in its stack's block, so any member of the
    stack that finds it can send through it; asking a router for a relay
    takes the relay key in its container's port directory.
- **Legacy links reach across stacks the same way.** A `--link` on the
  default bridge network, from one `docker run` container to another, each
  its own stack, gives the linking container Docker's variables and
  `/etc/hosts` entries for the child, and its router relays to the child
  as to a member of a network they share. Without a link, containers on
  the default bridge network reach one another only within a stack, where
  Docker lets any two reach each other by address. A link on a
  user-defined network, as Compose's `links:` makes, is an alias the
  linking container alone resolves. The stack's egress proxy, which an
  HTTP client may send a request for the alias to, cannot tell its
  containers apart, so it resolves any link's alias for any of them.
- **A host process that later binds a port inside a stack's block is
  reachable from that stack.** The allocator skips ports in use when it
  assigns them, but cannot stop other host software from binding them later.
- **Internet access goes through a proxy.** Seatbelt cannot express "any
  host but this machine", so containers reach the internet only through
  their stack's egress proxy, which refuses destinations on the host: HTTP
  and HTTPS for software that honours `HTTP_PROXY`/`HTTPS_PROXY`, and any TCP
  through its SOCKS5 side for software that honours `ALL_PROXY` or takes a
  SOCKS5 proxy. UDP and DNS from the container do not leave the machine; the
  proxy resolves names. `fwsd -egress=false` turns the proxy off.
- **A published UDP port's concurrent clients are bounded by the stack's
  block.** Each client's flow sends to the container from a port of the
  stack's block, the only ports a container may send to, so a stack serves
  as many UDP clients at once as its block has ports to spare; a flow ends
  after 90 seconds without traffic, as with Docker's userland proxy. SCTP
  ports cannot be published.
- **A container's ports are its loopback host ports, presented.** The
  container library binds a program's wildcard or loopback listener on a
  container port to that port's host port on `127.0.0.1`, connects a
  `localhost` client of a container port to its host port, and reports the
  container's addresses back; a dual-stack IPv6 socket used for IPv4 becomes
  an IPv4 socket that reads as IPv6, because Seatbelt matches loopback as
  `127.0.0.1` and `::1` and not as `::ffff:127.0.0.1`. Its limits:
  - Only ports the container exposes or publishes have host ports. A TCP
    listener or UDP socket on any other port listens on a Unix socket
    instead, which the container reaches on `localhost` and its stack's
    members reach by name, through the container's router; it is published
    nowhere, as in Docker. A listener on an address other than loopback's,
    the wildcard or the container's own is refused. A bind to port 0 takes
    a free port of the range macOS draws ephemeral ports from, 49152 to
    65535, the same way, as Seatbelt lets the kernel bind none but the
    stack's ports.
  - A UDP socket on such a port sends and receives with the socket and
    file calls, connected or not. Of IP-level control messages it takes
    those that report a datagram's destination, `IP_RECVDSTADDR`,
    `IP_PKTINFO` and `IPV6_RECVPKTINFO`, and sends with none: a reply
    leaves from the port the socket is bound to. Other IP options on it
    set nothing, as there are no packets for them to shape.
    The router gives each such port a flow port of the stack's block for as
    long as the container runs, so a stack holds as many of them as its
    block has ports to spare.
  - A dual-stack listener listens on `127.0.0.1` alone. An IPv6 client of
    `::1` reaches it when nothing holds the port on IPv6 loopback, and reads
    both its ends as `::1`, but the server sees that client as
    `::ffff:127.0.0.1`. An IPv4-only listener takes a client of `::1` too,
    where Docker would refuse it.
  - Programs that load no library, as Apple's do not, see the host ports.
- **Unix socket paths are capped at 104 bytes** on macOS, as the
  container sees them. A Unix socket at a container path,
  `/var/run/mysqld/mysqld.sock` say, binds, connects and takes datagrams
  at its host path in the container's root, by its directory where that
  path is longer. A datagram from a socket bound at such a path reads as
  sent from its last component, unless the receiver connected to it.

## Resources and identity

- **Limits are enforced by a supervisor, not the kernel.** Each container's
  shim checks its processes every 100 ms:
  - `--memory` caps the sum of their physical footprints, which counts
    compressed and swapped memory, so `--memory-swap` may only equal it (or
    be -1). Past the limit the largest process is killed and the container
    reports `OOMKilled`; an allocation never fails first, and a burst within
    100 ms can overshoot. `--memory-reservation`, swappiness and
    `--oom-kill-disable` are refused.
  - `--cpus` and `--cpu-quota` stop the processes with `SIGSTOP` once they
    use more than the quota and continue them once it has paid the excess
    back: a parent sees its children stop and continue, as with job
    control. `--cpu-shares` sets the nice value, and since only root lowers
    one, more shares than the default run at the default priority.
    Cpusets and real-time scheduling are refused.
  - `--pids-limit` counts processes, not threads as a pids cgroup does.
    The container library fails a `fork` or `posix_spawn` at the limit
    with `EAGAIN`; the supervisor kills a process two racing forks let
    past it, and one forked by a program that loads no library or through
    `vfork`.
  - `--blkio-weight` picks a disk I/O policy tier (standard, utility,
    throttle); per-device rates are refused, and a running container's
    weight cannot change, as macOS lets a process set only its own policy.
  - `--ulimit` sets nofile, fsize, cpu, core and stack; nproc, which macOS
    counts across every process of the user, and data and as, which it
    does not enforce, are refused.
- **Each container costs a supervisor process**, `fws-shim`, held under
  16 MiB of memory footprint by the test suite.
- **`docker pause` stops the container's processes with SIGSTOP**, not a
  cgroup freezer; TCP connections to a paused container are still accepted
  by the kernel and wait for it to resume.
- **`docker update` changes restart policies and limits**, the limits of a
  running container at once, except its block I/O weight.
- **Live restore keeps containers running across an engine restart** with
  `fwsd -live-restore`. Without it the engine stops its containers when it
  stops, and ends any a crashed engine left running when it starts.
- **`docker top` and `docker stats` see the container's process groups**: its
  main process's and its running execs'. They read the host's `ps`, as dockerd
  does, so commands show their host paths. Stats report CPU time, CPU
  throttling, the memory footprint the limit holds, the limits, process
  count, and the bytes the container's processes read from and wrote to
  disk, as the kernel counts them per process. A process that starts and
  ends between two samples, a second apart while `docker stats` streams,
  leaves its disk I/O out. Without a limit the memory limit shown is the
  machine's memory.
- **Network I/O is the kernel's per-socket count** of the container's TCP and
  UDP sockets and its router's, read from macOS's network statistics, the
  counters `nettop` shows. It reports one interface, `eth0`, and leaves out
  connections between two of the container's own sockets, as Docker's
  `eth0` leaves out its loopback. Its limits:
  - The bytes are payload bytes; Docker's count adds packet headers.
  - The kernel counts a socket against the process that last used it, and
    the shim tells a new socket's process apart as the kernel announces the
    socket. A process that ends before that leaves its socket out.
  - A UDP datagram between two of the container's own sockets counts, as
    UDP sockets have no peer the kernel names.
- **`docker cp` copies the container's view**, mounts included, but refuses
  paths that resolve outside the container's root and mounts.
- **Testcontainers' reaper runs from the catalog's darwin build of Ryuk**,
  tagged under the name Testcontainers runs (see the README). The build
  listens on loopback, as the container contract asks, and its image's `nc`
  checks a container port through its assigned host port for Testcontainers'
  in-container port check.
- **tmpfs mounts are directories** under the container's state, emptied at
  each start, not memory-backed. A tmpfs volume (`--opt type=tmpfs`) is a
  directory too, emptied when a container first mounts it; size and
  ownership options are refused, as nothing would hold them. The one
  memory-backed filesystem an unprivileged user can make, an `hdiutil` RAM
  disk, mounts system-wide under `/Volumes`, in Finder's sight.
- **Local volumes bind host directories or nothing else.** The local
  driver's `type=none,o=bind,device=<dir>` binds a host directory, which
  must exist; other filesystem types need root and are refused.
