# Limitations

Most limits here follow from two constraints, no root and no virtual
machine, and from what macOS offers an unprivileged process under them.
The rest are marked *not built yet*: they need engineering, not
privilege. The project's non-goals are in [PLAN.md](PLAN.md#non-goals).

## Images

- **macOS-native images only.** Nothing on macOS runs Linux ELF binaries
  without a VM, so stock Docker Hub images do not run
  ([PLAN.md](PLAN.md#non-goals)).
- **Images follow the container contract.** A service finds its root and
  ports at their container paths and port numbers through the container
  library, or reads them from `SANDBOX_*` variables (see the README).
  Software the library cannot load into needs an entrypoint that
  configures it from the variables.
- **Only the engine's platform.** A `--platform` or `FROM --platform`
  other than `darwin/<arch>` of the engine is refused. Pulling or loading
  a multi-platform index stores its `darwin/<arch>` image alone, and a
  BuildKit build's attestations stay out of the engine's store, as
  dockerd's classic image store leaves them out.
- **Disk figures are estimates.** An image's root is made of clones that
  share blocks with its cached layers, so `docker system df`, an image's
  size and what a prune reports reclaimed count more than the disk gave
  up. A layer with a hard link into a lower layer is unpacked for each
  image that holds it rather than shared.
- **`docker commit`, `docker diff` and `docker ps -s` read a file** whose
  size and whole-second modification time match the image's, to tell
  whether it changed, only where it, or its directory, changed after the
  container was created: a stamp of when its root was cloned spares the
  rest. A container created by an earlier engine has no stamp, and each
  such file of it is read.
- **VS Code's Dev Containers extension installs a Linux server.** The Dev
  Containers CLI works (`devcontainer up`, `exec`), and VS Code's macOS
  server runs in a dev container. The extension itself creates the
  container and runs its commands in it, then installs VS Code's server
  for Linux whatever the container runs, which no darwin container can
  run. With the macOS server of the window's commit put where the
  extension looks (`/vscode/vscode-server/bin/linux-arm64/<commit>` in its
  `vscode` volume) it skips the install, starts that server in the
  container and connects the window to it. Seeding the volume by hand is
  the only way yet: *not built yet* is the engine putting the macOS server
  there itself.
- **Catalog images are built by Nix**, mostly from source rather than the
  vendors' binaries; MongoDB and Vault are the vendors' own macOS builds.
- **Catalog images differ from upstream's where Linux parts are missing.**
  - `fws/kafka` is Apache Kafka in Confluent's layout, without
    Confluent's own components such as the REST Proxy;
    `KAFKA_TOOLS_LOG4J_LOGLEVEL` sets nothing.
  - `fws/meilisearch` serves no mini-dashboard and has no `meilitool`.
  - `fws/busybox` holds separate GNU and other darwin programs, not
    BusyBox's applets, so options differ; its `nc` only checks a port
    (`nc -z`), and it has no `ps`, `top` or `vi`. `fws/base` is not
    Alpine and has no package manager: add programs with `COPY` or by
    downloading darwin builds.
  - `fws/qdrant` serves the older web UI nixpkgs builds, not the latest.
  - `fws/neo4j` is the Community Edition.
  - `fws/opensearch` is the minimal distribution, without the bundled
    plugins (security, k-NN, ML Commons), so `DISABLE_SECURITY_PLUGIN`
    and the demo configuration act on nothing.
  - Kubernetes' images carry `/bin/sh` and `/bin/nc`, where upstream's
    are distroless; there is no kube-proxy or CoreDNS image (see
    [Kubernetes](#kubernetes)).
- **BuildKit builds darwin images on one worker.** Multi-platform builds
  and `# syntax=` frontends other than Docker's own are refused (gateway
  frontends ship as Linux images); `# syntax=docker/dockerfile:<version>`
  uses the vendored built-in Dockerfile frontend. An SSH mount's socket
  belongs to the engine's user whatever `uid` it names.
- **The classic builder (`DOCKER_BUILDKIT=0`) takes no heredocs**, as
  Docker's classic builder takes none; BuildKit builds take them.
- **Build steps run under Seatbelt** with internet through a build-only
  egress proxy (none with `--network none`); shell-form `RUN` needs
  `/bin/sh` in the image, as in Docker.
- **`docker commit` and `docker export` leave volumes out**, as Docker
  does.

## Isolation

- **Not a security boundary against hostile code.** Containers run as
  the engine's user; Seatbelt keeps stacks apart and the host's data out
  of reach, against mistakes rather than attackers
  ([DESIGN](docs/DESIGN.md#what-it-is-not)).
- **A container's view is presented by a library, not the kernel.** The
  container library maps absolute paths into the container's root and
  presents its user, groups, hostname and domain name, so `--user`,
  `USER`, `exec -u`, `--group-add`, `--hostname`, `--domainname` and
  `-w` behave as in Docker. Its limits:
  - A path names the root's file when the root holds it, or its parent;
    any other path, such as `/dev` or the system frameworks, names the
    host's. A container's `/proc` is an empty directory: no process is
    listed there, and `/proc/self/ns/mnt` names the container's one mount
    namespace. Symbolic links resolve inside the container, as under chroot;
    a path whose links loop fails with `ENOENT` where Linux gives
    `ELOOP`.
  - Programs Apple signs, statically linked programs and ones making
    system calls without libSystem load no library and see host paths,
    pids, ports and the host's network. A program signed with the
    hardened runtime, as vendors' macOS downloads are, is made to load it
    when a container process executes it: its signature is replaced by an
    ad hoc one without the runtime's flags, in the container's root or a
    writable mount. On a read-only mount it stays as it is and loads
    none.
    Catalog images carry their own `/bin/sh` and `/bin/nc` so `system()`,
    `popen()` and port checks keep the library.
  - The identity is presented, not enforced: every process runs as the
    engine's user, so a non-root user can do what root can. An image's owners and a
    `chown` are recorded in an extended attribute that `stat` reports,
    and setuid, setgid and sticky bits are presented and never take
    effect.
  - The library reserves the `nodump` file flag (`UF_NODUMP`) to mark
    files whose owner or mode it records; containers never see it and
    cannot set or clear it.
  - A script's `#!` interpreter runs from the root when the root holds
    it, else from the host.
- **`PATH` and `HOME` hold host paths**, in the container's root, so
  programs without the library find the container's commands; `which`
  and `echo $HOME` print the host's paths.
- **Processes are presented, with host pids but pid 1's.** There is no
  unprivileged PID namespace. Through the library a container lists only
  its own processes and sees its main process as pid 1 (`kill -HUP 1`
  works); Seatbelt denies signalling or inspecting any other process. A
  program without the library, such as Apple's `ps`, sees host pids and
  every process of the user ([DESIGN](docs/DESIGN.md#accepted-exceptions)).
- **A stop kills every process the main process started**, daemons that
  start their own session included, as macOS holds the main process
  responsible for them
  ([DESIGN](docs/DESIGN.md#3a-isolation-and-limits-without-cgroups)).
- **IPC is confined by name, presented for System V.** POSIX shared
  memory and semaphore names get a per-container prefix
  (`SANDBOX_IPC_PREFIX`) under private IPC; a name too long for macOS's
  31 bytes becomes the prefix and a hash. System V keys are hashed with
  the prefix, which Seatbelt cannot enforce: a program without the
  library, or one guessing a hashed key, reaches another container's
  objects. `--ipc none` denies all IPC; `--ipc host` lifts confinement.
- **`--shm-size` caps POSIX shared memory by a ledger.** `ftruncate`
  fails with `ENOSPC` past the size (64 MiB by default). An object counts
  until it is unlinked; each start removes the last run's objects. There
  is no `/dev/shm` directory to list.
- **Namespace modes are presented where the library can present them.**
  `--uts host` shows the host's hostname. `--network container:` shares
  the other container's ports, address, hostname and stack, and its
  listeners go when that container stops. `--pid host` lists and signals
  every process of the engine's user by host pid. `--pid container:`
  shares process listings and pid 1 through the shims, for programs that
  load the library; the joining container ends with the other.
  `--userns` is refused: there are no user namespaces.
- **Capabilities grant nothing to drop.** `--cap-drop` is accepted and
  changes nothing. `--cap-add` beyond Docker's default set needs root and
  is refused, except `IPC_LOCK`: any macOS process may lock memory,
  bounded by the system's wired-memory limits, not by `RLIMIT_MEMLOCK`.
  Seccomp, AppArmor and SELinux options are refused except
  `unconfined`/`disable`, which are accepted and change nothing.
- **The macOS system tree is readable** (`/System`, `/usr/lib`,
  `/usr/share`): binaries cannot run without it
  ([DESIGN](docs/DESIGN.md#accepted-exceptions)).
- **The host's shells are executable**: `/bin/sh`, `/bin/bash`,
  `/bin/zsh`, `/bin/dash` and `/usr/bin/env`, because `popen()` and
  `system()` hard-code `/bin/sh`, which hands off to the others.

## Networking

- **No host network mode.** `--network host` is refused, as every
  container is confined to its own stack's ports, except inside a
  privileged container's nested engine (see `--privileged`).
- **Bridge networks and the default IPAM driver only.** Other network
  drivers (overlay, macvlan, ipvlan), other IPAM drivers and IPAM options
  are refused: there is no network stack to give them.
- **ICMP reaches the virtual network alone.** An unprivileged `ping` of
  loopback, the container's own address or a member is answered by the
  library; an outside host over IPv4 is pinged by the container's router
  when the container has internet access. Otherwise, and for any outside
  IPv6 host, it fails with `ENETUNREACH`. Raw sockets need root.
- **Container addresses are presented, not routed.** Members of a stack
  have addresses in `127.88.0.0/16`, and on a network with a subnet one of
  that subnet; the default bridge network holds `172.17.0.0/16`, where
  `--ip` is refused, as Docker refuses it. Networks may enable IPv6. The
  kernel never sees these addresses:
  - A program without the library resolves no member name; it uses
    `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
  - Servers see clients connect from `127.0.0.1`; a UDP server sees an
    IPv6 client at its IPv4-mapped address.
  - A network takes any number of IPv4 and IPv6 subnets; a container
    takes an address of each family from the first subnet with room, or
    the one holding the address it names. On an IPv6-only network
    (`enable_ipv4: false`) members hold, report and resolve to IPv6
    addresses only, and its interface lists the IPv6 one alone. There is
    no link-local IPv6 address.
  - A container keeps its addresses while it exists, where Docker frees
    a stopped container's.
  - A network without a subnet reports `127.88.0.0/16`, a loopback block,
    so a program that advertises its first global address (Kubernetes'
    API server, SeaweedFS) finds none there: create it with a subnet.
  - A user-set MAC address is refused; each address gets a MAC of
    Docker's form, derived from it, on no interface.
- **A container's interfaces are presented.** `getifaddrs`, the
  `SIOCGIF*` ioctls (IPv4 only) and the routing sysctl list loopback and
  `eth0` onwards. The kernel knows no `eth0`: binding to its index or
  joining a multicast group on it fails.
- **A stack's block is one reachability domain.** Seatbelt lets a
  container reach every port of its stack's block, so within a stack
  network membership is presented, not enforced: a program without the
  library reaches any member's host port.
- **Stacks meet on a shared network through their routers.** A datagram
  across stacks uses a relay port of the sender's router, so concurrent
  senders are bounded by the other stack's block; a relay outlives a
  disconnect for 90 idle seconds. A program without the library reaches
  other stacks only through the egress proxy.
- **Legacy links work across stacks the same way.** Without a link,
  containers on the default bridge network reach one another only within
  a stack, where Docker lets any two reach each other by address.
- **A host process that later binds a port in a stack's block is
  reachable from that stack.** The allocator cannot stop it.
- **Internet access goes through a proxy.** Seatbelt cannot express "any
  host but this machine", so containers reach the internet only through
  their stack's egress proxy: HTTP(S) via `HTTP_PROXY`/`HTTPS_PROXY`, any
  TCP via its SOCKS5 side (`ALL_PROXY`). UDP does not leave the
  machine; DNS leaves it only for a container given `--dns`,
  `--dns-search` or `--dns-option`, whose router, outside the sandbox,
  queries the servers of its `resolv.conf`. `fwsd -egress=false` turns the proxy off.
- **Published UDP clients are bounded by the stack's block**, one flow
  port each, ended after 90 idle seconds. SCTP ports cannot be published:
  macOS has no SCTP.
- **A container's ports are its loopback host ports, presented.** The
  library binds a wildcard listener, or one on the container's address,
  to the port's host port on `127.0.0.1`. Its limits:
  - Only exposed or published ports have host ports; a listener on any
    other port, and a bind to port 0, listens on a Unix socket reached
    through the router. A listener on another address is refused.
  - A UDP socket on such a port takes `IP_RECVDSTADDR`, `IP_PKTINFO` and
    `IPV6_RECVPKTINFO`; other IP options set nothing.
  - A dual-stack listener listens on `127.0.0.1` alone; its server sees an
    IPv6 loopback client as `::ffff:127.0.0.1`, and an IPv4-only listener
    takes `::1` clients where Docker would refuse them.
  - A TCP listener on loopback alone is unpublished, as in Docker, but a
    stack member still reaches it through the router; beside a listener
    on the container's own address it fails with `EADDRINUSE`, where
    Linux allows both.
  - Programs without the library see the host ports.
- **Unix socket paths are capped at 104 bytes** on macOS, as the
  container sees them; a longer host path binds through the mount or root
  below it, so a datagram's sender reads as the mount's path.

## Kubernetes

- **No kube-proxy or CoreDNS.** Kube-proxy programs Linux iptables;
  `fws-kube-proxy` serves Services and the library resolves cluster
  names.
- **The kubelet applies no Linux settings.** A darwin node sends the
  runtime no Linux resources or security context, so `resources.limits`
  and `securityContext.runAsUser` take no effect.
- **The CRI refuses checkpoints**: macOS cannot save a running
  process's state. `UpdatePodSandboxResources` changes nothing, as
  containerd's does not: the engine limits each container. Pod metrics
  count CPU time in whole seconds. A pod's search domains and options go
  into its `resolv.conf`, and the library completes the cluster names it
  answers with them; its DNS servers, which name cluster DNS, go unused,
  and other names resolve through the host's resolver.
- **Pod networks hold the node's pod CIDR.** A pod takes its node's pod
  network addresses, of each family of the CIDR, an IPv6 one alone on an
  IPv6 CIDR; other nodes' pods reach it through the routers, and the
  host's engine does not route to it. A changed CIDR applies once the
  node's pods are gone.
- **Services carry TCP and UDP.** Cluster IPs, IPv4 and IPv6 or both,
  external IPs and load balancers' addresses answer alike, and node ports
  carry TCP and UDP. `sessionAffinity: ClientIP` keeps a pod, known by
  its address on the pod network, on its endpoint until the timeout;
  clients on the node's loopback count as one client, and a UDP flow a
  pod's router opened before its Service gained affinity keeps none.
  SCTP ports are not served: macOS has no SCTP. An `ExternalName`
  Service's name is an alias of the name it gives, as a CNAME record is.
  A Service's short name answers in every namespace, its own namespace's
  first in a pod, whose search domains name it. Changes arrive within the
  proxy's poll, a second by default.
- **kind runs on `fws/kind-node` only.** Nodes run no systemd or
  containerd: `ctr` supports `images import`, `tag` and `ls`, and
  `journalctl` reads the node's logs. IPv6 and dual-stack clusters run;
  `containerdConfigPatches` are not supported (*not built yet*).
- **`fws/k3s` is kubeadm's cluster under k3s's names.** `k3s agent`,
  joining servers, Traefik, ServiceLB, the Helm controller, the embedded
  registry and etcd snapshots are absent. The node needs an address
  outside loopback: use the default bridge network or one with a subnet.

## Resources and identity

Limits are enforced by the container's supervisor, `fws-shim`, every
100 ms, or by the spawn helper and the kernel's per-process limits, not
by a cgroup ([DESIGN](docs/DESIGN.md#3a-isolation-and-limits-without-cgroups)).

- **`--memory`** caps each process's physical footprint in the kernel
  (jetsam's fatal limit, at once) and the processes' sum in the shim
  (every 100 ms); past either the process, or the largest, is killed and
  the container reports `OOMKilled`. An allocation never fails first;
  several processes together can overshoot the sum between two samples,
  and a process forked without an exec, which the kernel gives no limit,
  is held by the sum alone.
  `--memory-swap` may only equal the limit (or be -1).
  `--memory-reservation`, swappiness and `--oom-kill-disable` are
  refused.
- **`--cpus` and `--cpu-quota`** stop the processes with `SIGSTOP` over
  quota, so a parent sees its children stop and continue.
  `--cpu-shares` sets the nice value, and since only root lowers one,
  more shares than the default run at the default priority. Cpusets are
  refused (macOS lets no process choose CPUs), as is real-time
  scheduling: Docker's real-time budget is a cgroup budget with no macOS
  counterpart.
- **`--pids-limit`** counts processes, not threads. `fork` and
  `posix_spawn` fail with `EAGAIN` at the limit; a process forked past
  it by a race, `vfork` or a program without the library is killed.
- **`--blkio-weight`** picks a disk I/O tier (standard, utility,
  throttle). On a running container an update reaches every process the
  container starts after it, as the library sets each one's as it
  starts; processes already running keep their tier until the container
  restarts, as macOS lets a process set only its own policy. Per-device
  rates are refused.
- **`--ulimit`** sets nofile, fsize, cpu, core and stack, and memlock
  only unlimited. Refused: nproc (macOS counts it per user); data, as and
  a finite memlock (macOS does not enforce them); rss, locks, sigpending,
  msgqueue, nice, rtprio and rttime (macOS has no such limit).
- **Machine guard.** Under critical memory pressure, or under 2 GiB free
  disk, the engine pauses the container using the most, one every two
  seconds, and resumes it once the pressure is normal or 4 GiB are free.
  `fwsd -machine-guard=false` turns this off.
- **Every container shares the user's process limit**, held an eighth
  below `kern.maxprocperuid`, and runs under a QoS clamp (`utility` by
  default, `fwsd -qos-clamp`), below the user's interactive work.
- **`--init` runs the engine's own init** at `/sbin/docker-init`.
- **`--privileged` gives a container an engine of its own, nothing
  more.** It runs Docker in Docker (`fws/dind`): the outer engine runs
  the nested engine's containers, as macOS refuses a second Seatbelt
  profile in a sandboxed process; no devices, capabilities or mounts are
  granted. The nested engine publishes TCP and UDP ports and takes four
  port blocks cut into forty stacks.
  `docker exec --privileged` is refused outside a privileged container.
  Linux `docker:dind`, `kindest/node` and Podman need a VM.
- **A Podman pod shares its network, IPC and PID namespaces**, through
  an infra container that runs `fws/pause`, which must be in the store.
  The UTS namespace comes with the network; the user and cgroup
  namespaces cannot be shared, as containers have none. `generate kube`,
  `play kube`, checkpoints and quadlets are not served.
- **DNS goes through the container's router.** A
  container with `--dns`, `--dns-search` or `--dns-option` resolves the
  names its `/etc/hosts` and its stack do not answer through its router,
  which needs internet access. `getaddrinfo_async_start` answers as
  `getaddrinfo` does for the names the container answers itself, and
  `DNSServiceGetAddrInfo`, whose mDNSResponder socket a container cannot
  reach, for every name. Network.framework, and so `URLSession`,
  resolves every name as `getaddrinfo` does in the container, through a
  stand-in for the private dnssd client it asks mDNSResponder with, which
  a container cannot reach, and connects through `connectx`, which the
  library routes as it does `connect`; the remote endpoint it reports is
  the host port or router the connection went through. A program that sends
  DNS itself (`dig`, Go's pure resolver) to a server outside the
  machine, over UDP or TCP, at an IPv4 or IPv6 address, reaches the
  router instead, which answers the A and AAAA
  questions for its stack's names and passes every other query to the
  servers of the container's `resolv.conf`, given internet access, and
  refuses it without; the answer comes as from the server asked, though
  `dig @server` is answered by the container's servers. Of the options,
  `ndots`, `timeout` and `attempts` take effect. A server in the stack,
  or a Service, cannot serve.
- **Settings without a macOS counterpart are refused**:
  - DNS settings in an engine nested in a privileged container, and a
    DNS server on loopback;
  - `--sysctl`, `--userns`, runtimes other than runc, log drivers other
    than `json-file` and `local` (including `none`);
  - devices, `--gpus`, device cgroup rules, `--cgroup-parent`, cgroup
    sharing, `--oom-score-adj`, `--storage-opt`, Windows-only limits and
    isolation technologies other than the default;
  - volume drivers other than `local` (`--volume-driver` too), and mount
    types other than bind, volume, image and tmpfs.
- **Some settings are accepted and change nothing**, as each asks for no
  more confinement than a container has: `--cap-drop`,
  `seccomp`/`apparmor=unconfined`, `label=disable` and `--cgroupns`.
  Bind propagation holds as `rslave` whatever is asked: mounts the host
  makes below a bind's source show in the container, which can make
  none of its own.
- **`docker pause` uses `SIGSTOP`**, not a cgroup freezer; connections
  to a paused container are still accepted and wait.
- **`docker update` changes restart policies and limits**, live for a
  running container, except its block I/O weight and a raise of its
  memory limit: the kernel holds each running process to the limit it
  started under, which no process may raise, so a raise is refused until
  the container is stopped. Lowering the limit, or setting one where
  there was none, holds at once through the shim. Kubernetes' in-place
  resize of a pod's memory upward fails the same way.
- **Live restore needs `fwsd -live-restore`.** Without it the engine
  stops its containers when it stops, and ends any a crash left running.
- **`docker top` and `docker stats` read the host's `ps`**, so commands
  show host paths. A process that starts and ends between two samples, a
  second apart, leaves its disk I/O out. Without a limit the memory
  limit shown is the machine's memory.
- **Network I/O is the kernel's per-socket payload count** on one
  interface, `eth0`, without packet headers. A process that ends before
  the shim sees its socket leaves it uncounted, and UDP between two of the
  container's own sockets counts.
- **`docker cp` refuses paths** that resolve outside the container's
  root and mounts.
- **tmpfs mounts are directories**, emptied at each start, not
  memory-backed. `rw`, `ro`, `noexec` (through the profile) and `mode`
  hold, as do `nosuid` and `nodev`, as an unprivileged process makes no
  setuid file or device; `size`, `nr_inodes`, `uid` and `gid` are
  refused, on tmpfs volumes too. The one memory-backed filesystem an
  unprivileged user can make, an `hdiutil` RAM disk, mounts system-wide
  under `/Volumes`.
- **A bind's missing source is created**, as Docker does; a read-only
  one the host cannot hold, such as `/lib/modules` on the sealed system
  volume, mounts an empty directory.
- **Local volumes bind host directories only** (`type=none,o=bind`);
  other filesystem types need root and are refused.

## API surface

- **Not served:** swarm, services, nodes, secrets, configs, plugins and
  image search. The engine serves what Docker Compose, the Docker CLI
  and Testcontainers use
  ([DESIGN](docs/DESIGN.md#8-docker-engine-api-surface)).
