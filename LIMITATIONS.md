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
- **Catalog images are built by Nix**, mostly from source rather than the
  vendors' binaries. Where a build records itself, as MySQL's and
  MariaDB's compilation comment does, the catalog builds it as the vendor
  does. MongoDB and Vault are the vendors' own macOS builds, which nixpkgs
  packages as they are.
- **`fws/kafka` is Apache Kafka in Confluent's layout.** It carries the
  `/etc/confluent/docker` scripts, their `ub` and Kafka's tools without
  their `.sh`, which Testcontainers' module and Compose files written for
  Confluent's images rely on, and none of Confluent's own components, such
  as the REST Proxy `confluentinc/confluent-local` runs. Kafka's tools log
  through Kafka's own `tools-log4j2.yaml`, so `KAFKA_TOOLS_LOG4J_LOGLEVEL`
  sets nothing; their level is the warning level Confluent's default is.
- **`fws/meilisearch` serves no mini-dashboard and carries no `meilitool`.**
  nixpkgs builds Meilisearch without the dashboard, which its build would
  download, and builds the server alone.
- **`fws/busybox` is not BusyBox, and `fws/base` is not Alpine.** BusyBox
  and Alpine are Linux userlands, which run only in a VM. `fws/busybox`
  holds the commands scripts run in BusyBox's image as separate programs,
  GNU's and others nixpkgs builds for darwin, so their options are those
  programs' rather than BusyBox's applets'; its `nc` checks a port
  (`nc -z`) and does nothing else, and it has no `ps`, `top` or `vi`.
  `fws/base` has no package manager: `apk` and `apt` install Linux
  binaries. An image built `FROM` it adds programs with `COPY` or by
  downloading darwin builds.
- **`fws/qdrant` serves nixpkgs' web UI.** Qdrant's image fetches the web
  UI's latest release when it is built; the catalog serves the release
  nixpkgs builds from source, an older one, so rebuilds serve the same
  dashboard.
- **`fws/neo4j` is the Community Edition.** Its official entrypoint's
  behaviour is reimplemented in Go: `NEO4J_PLUGINS` installs APOC from the
  distribution and downloads the others from their versions files, and
  `EXTENSION_SCRIPT` runs in bash, whose environment the server then
  inherits, as when the official script sources it. The image is not
  Debian-based, so it prints no warning about Debian releases.
- **`fws/influxdb`'s entrypoint is reimplemented in Go.** The official
  script needs `gosu`, which nixpkgs builds only for Linux, and `dasel` 2,
  whose successor in nixpkgs reads other arguments; the Go entrypoint does
  what the script does, and still runs the user's scripts with `run-parts`.
- **`fws/opensearch` is OpenSearch's minimal distribution on nixpkgs'
  JDK.** The official image carries the full bundle, whose plugins (the
  security plugin, k-NN, ML Commons and the rest) take about a gigabyte,
  some with Linux-only native libraries; the catalog carries the server
  and its modules, so `DISABLE_SECURITY_PLUGIN` and the demo
  configuration have no plugin to act on, and `opensearch-plugin install`
  adds plugins as on any distribution. The official image runs the JDK
  bundled for Linux; the catalog links nixpkgs' at the bundled JDK's
  path, `/usr/share/opensearch/jdk`.
- **Kubernetes' images are its control plane, kubectl and a kubelet.**
  The catalog carries the API server, the controller manager, the
  scheduler and kubectl, which a control plane of containers runs as
  kubeadm's static pods do, and a kubelet for macOS; it carries no
  kube-proxy, which programs Linux iptables. Upstream's images are
  distroless, without a shell; the catalog's carry `/bin/sh` and
  `/bin/nc`, as all its images do.
- **The kubelet applies no Linux settings to its containers.** On a darwin
  node it sends the runtime no Linux resources or security context, as
  upstream's kubelet sends none on an operating system it has no settings
  for: a container's `resources.limits` and `securityContext.runAsUser`
  take no effect, and the image's user runs it.
- **A pod's address is its node's.** A pod sandbox takes its address on
  the engine that runs it, which other nodes' and the host's engines do
  not route to; cluster networking is on the roadmap.
- **A container's view is presented by a library, not the kernel.** Every
  container process loads the engine's container library, which maps
  absolute paths into the container's root (as fakechroot does), presents
  the container's user and group (as fakeroot does), its users and groups
  from its own `/etc/passwd` and `/etc/group`, its supplementary groups
  (the user's memberships there and `--group-add`), and its hostname and
  domain name. So arguments, entrypoints, `CMD`, `--user`, `USER`,
  `exec -u`, `--group-add`, `--hostname`, `--domainname` and `-w` behave
  as in Docker. Its limits:
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
  - The library keeps the `nodump` file flag (`UF_NODUMP`) for itself: it
    marks the files whose owner or mode it records, so that `stat` reads
    the record only for them. A container's processes never see the flag
    and cannot set or clear it.
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

- **`PATH` and `HOME` hold host paths.** A container's `PATH` names its
  directories as the host sees them, in the container's root, and `HOME`
  its home there, so programs started without the container library, such
  as Apple's, find the container's commands. A program that prints
  them, as `which` and `echo $HOME` do, prints the host's paths.
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
- **`--shm-size` caps the container's POSIX shared memory.** The
  container library keeps a ledger of the sizes of the IPC namespace's
  shared memory objects, and `ftruncate` fails with `ENOSPC` past the
  size, 64 MiB unless the container asks for another, as a full
  `/dev/shm` refuses on Linux. An object counts until it is unlinked, not
  until its last mapping goes, and each start removes the objects the last
  run left, as each start of a Docker container mounts an empty
  `/dev/shm`. There is no `/dev/shm` directory to list.
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
  it. `--pid host` lifts the process confinement: the container lists and
  signals every process of the engine's user, by host pid, with no
  presented pid 1, and its pids limit still counts its own processes.
  `--pid container:` shares the other container's PID namespace through
  the two containers' shims: each lists the other's processes, signals
  them and reads their process information, and the other's main process
  is pid 1 to both; when it ends, so does every container that joined it.
  The shim carries only the calls Seatbelt refuses and the library knows,
  so a program that loads no library sees its own container's processes
  alone. `--userns` is refused.
- **Capabilities grant nothing to drop.** Containers run as the engine's
  unprivileged user, so `--cap-drop` changes nothing; `--cap-add` beyond
  Docker's default set needs root and is refused, except `IPC_LOCK`, whose
  power to lock memory macOS gives every process up to its
  `RLIMIT_MEMLOCK`. AppArmor, SELinux and
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
  has an address in `127.88.0.0/16`, and on a network created with a
  subnet (Compose's `ipam`, `docker network create --subnet`) an address
  of that subnet, the one `ipv4_address` or `--ip` names or the lowest
  free one. On a network with IPv6 (`enable_ipv6`, `--ipv6`) it holds an
  IPv6 address too, of the network's IPv6 subnet, or of a `/64` the
  engine takes from a unique local `/48` of its own, as dockerd does,
  where the network names none; `ipv6_address` or `--ip6` names one in a
  subnet the network was created with. The container library resolves a
  member's names to its address on the first network the two share, and
  over IPv6 to its IPv6 address on the first such network that gives one,
  sorting a unique local IPv6 address after IPv4, as RFC 6724 does. It
  reaches a member's ports through any of its addresses, so a client
  connects to `db:5432` as on a Compose network. The kernel never sees
  those addresses:
  - A program that loads no library resolves no member name; it uses
    `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
  - A server sees its clients connect from `127.0.0.1`, not from their
    member addresses.
  - A member's port without a host port is reached through its router,
    whose connections and datagrams its server sees come from `127.0.0.1`.
  - A socket that reached a member at its IPv6 address reads that member,
    and its own end, at their IPv6 addresses. A UDP server sees a client
    that sent over IPv6 at the client's IPv4-mapped address, as the
    router relays datagrams over IPv4.
  - A network takes one IPv4 subnet of the default IPAM driver, outside
    `127.0.0.0/8` and every other network's subnet, and with IPv6 one
    IPv6 subnet, outside loopback, link-local, multicast and IPv4-mapped
    addresses; more than one of either, and IPv6-only networks
    (`enable_ipv4: false`), are refused: the library reaches every member
    at an IPv4 address. A container has no link-local IPv6 address. A
    container keeps its
    addresses while it exists, where Docker gives a stopped container's
    back to the pool; `docker inspect` reports none for a stopped one, as
    Docker does. A network without a subnet reports `127.88.0.0/16`, the
    member addresses' block, which every such network shares. Those are
    loopback addresses, so a program that advertises its first global
    unicast address, such as Kubernetes' API server, finds none on such
    a network: create it with a subnet, as kind does.
- **A container's interfaces are presented.** `getifaddrs`, `if_nameindex`,
  the `SIOCGIF*` ioctls and the routing sysctl Go's `net.Interfaces` reads
  list the loopback interface and, on a network, `eth0` onwards, one for
  each of the container's addresses in the order of its networks' names,
  with its subnet's mask and a MAC address of Docker's form, and its IPv6
  address on that network, where the host's interfaces are. The `SIOCGIF*`
  ioctls answer for IPv4 alone. The kernel knows no `eth0`: binding a socket to
  its index, or joining a multicast group on it, fails.
  The member address is a loopback address, so a program that advertises
  its first non-loopback address finds none on a network without a
  subnet: SeaweedFS advertises `localhost`, where on Docker it advertises
  the container's address.
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
  - A program that loads no library cannot reach the other stack's members
    except through its stack's egress proxy, which reaches them for a
    member that shares the network, as the library does.
  - A relay's UDP port lies in its stack's block, where any member of the
    stack may send, but the relay takes datagrams only from its
    container's ports, and asking a router for a relay takes the relay key
    in its container's port directory.
- **Legacy links reach across stacks the same way.** A `--link` on the
  default bridge network, from one `docker run` container to another, each
  its own stack, gives the linking container Docker's variables and
  `/etc/hosts` entries for the child, and its router relays to the child
  as to a member of a network they share. Without a link, containers on
  the default bridge network reach one another only within a stack, where
  Docker lets any two reach each other by address. A link on a
  user-defined network, as Compose's `links:` makes, is an alias the
  linking container alone resolves, through the container library and
  the stack's egress proxy alike.
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
  A container's proxy variables carry credentials that name it to the
  proxy, which reaches, for a client that sends its stack's own traffic
  through it, the members that container shares a network with or links
  to; a client that drops the credentials reaches the internet alone.
- **A published UDP port's concurrent clients are bounded by the stack's
  block.** Each client's flow sends to the container from a port of the
  stack's block, the only ports a container may send to, so a stack serves
  as many UDP clients at once as its block has ports to spare; a flow ends
  after 90 seconds without traffic, as with Docker's userland proxy. SCTP
  ports cannot be published.
- **A container's ports are its loopback host ports, presented.** The
  container library binds a program's wildcard listener, or its listener
  on the container's own address, on a container port to that port's host
  port on `127.0.0.1`, connects a
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
  - A TCP listener on loopback alone listens on a Unix socket, as on a
    port without a host port, so that, as in Docker, the container reaches
    it on `localhost` and its published port does not: an entrypoint's
    setup server on `127.0.0.1` stays out of reach of a client waiting for
    the real server. Beside a listener on the container's own address it
    is refused with `EADDRINUSE`, where Linux would let the two coexist. A
    UDP socket on loopback alone still takes the host port.
  - A stack member reaches a listener on loopback alone on a port without
    a host port through the container's router, where Docker would refuse
    it: the router cannot tell the container's own clients from members'.
  - Programs that load no library, as Apple's do not, see the host ports.
- **Unix socket paths are capped at 104 bytes** on macOS, as the
  container sees them. A Unix socket at a container path,
  `/var/run/mysqld/mysqld.sock` say, binds, connects and takes datagrams
  at its host path in the container's root, by its path below the root,
  or the mount that holds it, where the host path is longer. A datagram
  from a socket bound at such a path reads as sent from its container
  path, found as the socket of that path below a mount or the root, so
  one path held by two of them reads as the mount's.

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
  - `--ulimit` sets nofile, fsize, cpu, core and stack, and memlock
    unlimited; nproc, which macOS counts across every process of the user,
    and data, as and a finite memlock, which it does not enforce, are
    refused.
- **`--init` runs the engine's own init**, at `/sbin/docker-init` as
  Docker places tini: the engine's helper binary, which by that name
  passes signals on to the command, reaps orphans and exits as the command
  does.
- **`--privileged` gives a container an engine of its own, nothing more.**
  It is how Docker in Docker runs (`fws/dind`): the engine nests another
  engine in the container, with its own containers, networks, volumes and
  images, and serves its API on a socket the container's `dockerd`
  relays. macOS refuses a second Seatbelt profile in a sandboxed process,
  so the outer engine runs the nested engine's containers, each in a
  sandbox of its own, and the privileged container gains no other right:
  no devices, capabilities or mounts. Its engine reads bind mounts as the
  container's own paths and publishes TCP ports in the container, on the
  container's host port for that port where it has one; it publishes no
  UDP ports. The nested engine takes four of the outer engine's port
  blocks for its stacks, and stops its containers when the container
  stops. A Linux `docker:dind`, KinD's `kindest/node` and Podman's own
  engine run Linux images and namespaces, which need a VM; a KinD node
  image built from macOS parts is on the roadmap (`PLAN.md`).
- **Podman's pods share no namespaces.** A pod's infra container, which
  holds the namespaces its containers share, runs Podman's pause image,
  which the engine does not have, so a pod is created with
  `--infra=false --share=`, as `podman-compose` creates its pods.
  `podman generate kube`, `play kube`, checkpoints and quadlets are not
  served.
- **Settings without a macOS counterpart are refused**, never ignored:
  DNS servers, search domains and options (a container resolves names
  through the host's resolver and reaches the internet through its
  stack's egress proxy), `--oom-score-adj`, `--storage-opt`, cgroup
  sharing and isolation technologies other than the default. `--cgroupns`
  takes Docker's values and changes nothing, as there are no cgroups to
  show.
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
  tagged under the name Testcontainers runs (see the README). Its image's
  `nc` checks a container port for Testcontainers' in-container port check,
  which on Linux reads `/proc/net/tcp`.
- **tmpfs mounts are directories** under the container's state, emptied at
  each start, not memory-backed. A tmpfs volume (`--opt type=tmpfs`) is a
  directory too, emptied when a container first mounts it; size and
  ownership options are refused, as nothing would hold them. The one
  memory-backed filesystem an unprivileged user can make, an `hdiutil` RAM
  disk, mounts system-wide under `/Volumes`, in Finder's sight.
- **Local volumes bind host directories or nothing else.** The local
  driver's `type=none,o=bind,device=<dir>` binds a host directory, which
  must exist; other filesystem types need root and are refused.
