# Limitations

Where the engine differs from Docker on Linux, and why. Almost every limit
follows from running with no root and no virtual machine, on what macOS
offers an unprivileged process. Non-goals are in
[PLAN.md](PLAN.md#non-goals).

## Images

- **macOS-native images only.** Nothing on macOS runs Linux ELF binaries
  without a VM, so stock Docker Hub images do not run.
- **Images follow the container contract.** A service finds its root and
  ports at their container paths and port numbers through the container
  library, or reads them from the `SANDBOX_*` variables
  ([IMAGES](docs/IMAGES.md#the-container-contract)). Software the library
  cannot load into needs an entrypoint that configures it from the
  variables.
- **Only the engine's platform.** A `--platform` or `FROM --platform` other
  than the engine's `darwin/<arch>` is refused. A multi-platform index
  stores its `darwin/<arch>` image alone; a BuildKit build's attestations
  stay out of the store, as dockerd's classic store leaves them out.
- **Disk figures are estimates.** A root is a clone sharing blocks with its
  cached layers, so `docker system df`, image sizes and prune totals count
  more than the disk gives up. A layer with a hard link into a lower layer
  is unpacked for each image that holds it.
- **`docker diff`, `commit` and `ps -s` compare files by size and
  whole-second modification time**, reading only those changed since the
  root was cloned.
- **Catalog images are built by Nix**, mostly from source; MongoDB,
  Vault, ClickHouse, Bun, Rust and Playwright's browsers are the vendors'
  own macOS builds. Where Linux parts are missing they differ from
  upstream's:
  - `fws/kafka` is Apache Kafka in Confluent's layout, without Confluent's
    own components such as the REST Proxy.
  - `fws/busybox` holds separate GNU and other darwin programs, not
    BusyBox's applets, so options differ; its `tar` is libarchive's, as
    macOS's is; its `nc` only checks a port (`nc -z`); it has no `ps`,
    `top` or `vi`. `fws/base` has no package manager: add programs with
    `COPY` or by downloading darwin builds.
  - `fws/playwright` carries Chromium's headless shell and Firefox. It
    has no full Chromium, which aborts as it starts under the container's
    profile, so `channel: 'chromium'` and headed Chromium are absent. It
    has no WebKit: Playwright's WebKit on macOS starts its web content,
    network and GPU processes as XPC services, which launchd runs outside
    the container's sandbox and library, and its builds differ by macOS
    release, which an image cannot choose for its host. Firefox's own
    sandboxes are off (`MOZ_DISABLE_*_SANDBOX`), as Chromium runs with
    `--no-sandbox`, and stay off only where a launch keeps the container's
    environment.
  - `fws/neo4j` is the Community Edition.
  - `fws/node` and `fws/rust` compile C and C++ with nixpkgs' clang, ld64
    and macOS SDK, not Xcode's: there is no `xcrun`, `xcodebuild` or
    Swift, and node-gyp is nixpkgs' build, which takes a fixed Xcode
    version rather than ask `xcodebuild`.
  - `fws/rust` has no rustup: its toolchain is the release's build for
    `aarch64-apple-darwin` alone, with rustfmt and clippy, so `rustup`
    commands and other targets are not available and a
    `rust-toolchain.toml` goes unread.
  - `fws/opensearch` is the minimal distribution with the security plugin,
    without the k-NN and ML Commons plugins, whose native libraries are
    built for Linux.
  - Kubernetes' images carry `/bin/sh` and `/bin/nc`, where upstream's are
    distroless; there is no kube-proxy or CoreDNS image.
- **BuildKit builds on one darwin worker.** Multi-platform builds and
  `# syntax=` frontends other than Docker's own are refused (gateway
  frontends ship as Linux images); `# syntax=docker/dockerfile:<version>`
  uses the vendored built-in frontend. An SSH mount's socket belongs to the
  engine's user whatever `uid` it names.
- **Build steps run darwin programs.** Every Dockerfile instruction
  builds; a `RUN` step runs what the darwin image holds, so one that runs
  a Linux package manager (`apt-get`, `apk`, `yum`) fails. Steps run under
  Seatbelt, with internet through a build-only egress proxy (none with
  `--network none`). The classic builder (`DOCKER_BUILDKIT=0`) refuses
  `RUN --mount`, `--network` and `--security`, and any option it does not
  implement or that is given twice, as dockerd's does.
- **Dev container features install darwin programs.** The containers.dev
  collection's features (`common-utils`, `docker-outside-of-docker`,
  `docker-in-docker`, `node`, `python` and the rest) install Linux
  packages or binaries and fail; a feature whose `install.sh` fetches or
  builds darwin programs works. Docker outside of Docker is the engine's
  socket in `mounts` with a darwin Docker CLI copied from `fws/dind`.
  VS Code's server in a dev container is its macOS build, so an extension
  published for Linux alone does not run there. The Dev Containers CLI
  applies `updateRemoteUserUID` on Linux hosts alone; here every process
  runs as the engine's user, who owns the workspace on the host.

## Isolation

- **Not a security boundary against hostile code.** Containers run as the
  engine's user; Seatbelt keeps stacks apart and the host's data out of
  reach, against mistakes rather than attackers
  ([DESIGN](docs/DESIGN.md#what-it-is-not)).
- **A container's view is presented by a library, not the kernel.** The
  container library maps paths into the root and presents the user,
  groups, hostname, domain name, `PATH` and `HOME`, so `--user`, `USER`,
  `exec -u`, `--group-add`, `--hostname`, `--domainname` and `-w` behave as
  in Docker. Its limits:
  - A path names the root's file when the root holds it, or its parent;
    any other path names the host's. `/System`, `/usr/lib`, `/dev`,
    `/private/var/db`, `/var/db` and `/Library/Apple` are always the
    host's, as the loader and libSystem read them there, so an image
    cannot put files in them: `fws-mkimage` refuses such a path, and
    `npm install -g` in the catalog's Node.js images goes to `/usr/local`.
    `/proc` is an empty directory. Symbolic links resolve inside
    the container, as under chroot; a link loop fails with `ENOENT`, not
    `ELOOP`.
  - Programs Apple signs, static binaries and programs making system calls
    without libSystem load no library and see host paths, pids, ports and
    network; they get `PATH` and `HOME` in the root, where they find the
    container's commands. A program signed with the hardened runtime is
    re-signed ad hoc when a container executes it, so it loads the
    library, except on a read-only mount. Catalog images carry their own
    `/bin/sh` and `/bin/nc`, so `system()`, `popen()` and port checks keep
    the library.
  - Identity is presented, not enforced: every process runs as the
    engine's user. Owners from an image or a `chown` are recorded in an
    extended attribute that `stat` reports; setuid, setgid and sticky bits
    are presented and never take effect. The library reserves the
    `UF_NODUMP` file flag to mark such files.
  - A script's `#!` interpreter runs from the root when the root holds it,
    else from the host.
- **Chromium-based applications take `--no-sandbox`.** macOS refuses a
  second Seatbelt profile to a sandboxed process, so Chromium's helpers
  cannot enter their own (`TestSecondSandboxRefused`); the container's
  profile confines them.
- **Processes have host pids, but pid 1.** There is no unprivileged PID
  namespace. Through the library a container lists only its own processes
  and sees its main process as pid 1 (`kill -HUP 1` works); Seatbelt
  denies signalling or inspecting any other, and reading the process
  table's listings (`kern.proc.all`, `uid`, `ruid`, `tty`), which the
  library asks the container's supervisor for instead. A program without
  the library, such as Apple's `ps`, lists nothing; one that knows another
  process's pid still reads its `kern.proc.pid` entry, and a listing of one
  process group or session stays readable.
- **A stop kills every process the main process started**, daemons in
  their own session included, as macOS holds the main process responsible
  for them.
- **IPC is confined by name.** POSIX shared memory and semaphore names get
  a per-container prefix (`SANDBOX_IPC_PREFIX`); a name past macOS's 31
  bytes becomes the prefix and a hash. System V keys are hashed with the
  prefix, which Seatbelt cannot enforce: a program without the library, or
  one guessing a hashed key, reaches another container's objects.
  `--ipc none` denies all IPC; `--ipc host` lifts confinement.
- **`--shm-size` caps POSIX shared memory by a ledger**: `ftruncate` fails
  with `ENOSPC` past it (64 MiB by default). There is no `/dev/shm` to
  list.
- **Namespace modes are presented.** `--uts host` shows the host's
  hostname. `--network container:` shares the other's ports, address,
  hostname and stack. `--pid host` lists and signals every process of the
  engine's user by host pid; `--pid container:` shares listings and pid 1
  for programs that load the library. `--userns` is refused.
- **Capabilities grant nothing to drop.** `--cap-drop` changes nothing.
  `--cap-add` beyond Docker's default set is refused, except `IPC_LOCK`.
  Seccomp, AppArmor and SELinux options are refused except
  `unconfined`/`disable`, which change nothing.
- **The macOS system tree is readable** (`/System`, `/usr/lib`,
  `/usr/share`), and the host's shells (`/bin/sh`, `bash`, `zsh`, `dash`,
  `/usr/bin/env`) are executable: binaries cannot run without them
  ([DESIGN](docs/DESIGN.md#accepted-exceptions)). Of the rest of the host
  a container reads only the time zone, the CA certificates and the
  services and protocols databases, as a root of its own would hold them;
  a container granted the user's session (`--security-opt seatbelt=gui`)
  reads the host but user data and other stacks' state.

## Networking

- **No host network mode**, except inside a privileged container's nested
  engine: every container is confined to its stack's ports.
- **Bridge networks and the default IPAM driver only.** Overlay, macvlan,
  ipvlan, other IPAM drivers and IPAM options are refused. Of the bridge
  driver's options, `enable_ip_masquerade=false` gives the network no
  internet access, `enable_icc=false` keeps its members from reaching,
  and resolving, one another over it, `host_binding_ipv4` is its members'
  default published address, the bridge's name is reported, an MTU
  changes nothing, and `fws.egress.allow` is an egress allow list (below);
  any other option is refused.
- **Container addresses are presented, not routed.** Members have
  addresses in `127.88.0.0/16`, or in their network's subnets (the default
  bridge holds `172.17.0.0/16`, where `--ip` is refused, as in Docker);
  networks may be IPv4, IPv6-only or dual-stack. The kernel never sees
  these addresses:
  - Servers see clients connect from `127.0.0.1`; a UDP server sees an
    IPv6 client at its IPv4-mapped address.
  - A program without the library reaches no member: its connections go
    through the container's router, which the library asks.
  - A container keeps its addresses while it exists. There is no
    link-local IPv6 address, and a user-set MAC address is refused.
  - A network without a subnet reports `127.88.0.0/16`, so a program that
    advertises its first global address (Kubernetes' API server,
    SeaweedFS) finds none: give it a subnet.
- **Interfaces and routes are presented.** `getifaddrs`, the `SIOCGIF*`
  ioctls (IPv4) and the routing sysctl list loopback and `eth0`, and the
  routing table (`netstat -rn`) the host's loopback routes, each IPv4
  subnet of the container's networks and a default route through its
  first network's gateway; IPv6 routes are the host's loopback ones alone.
  Binding to `eth0`'s index or joining a multicast group on it fails.
  Programs without the library see the host's.
- **A container's ports are its loopback host ports.** Only exposed or
  published ports have host ports; a listener on any other port, or port
  0, listens on a Unix socket reached through the router. A dual-stack
  listener listens on `127.0.0.1` alone. A loopback-only TCP listener is
  unpublished, as in Docker, but stack members still reach it. Programs
  without the library see the host ports.
- **Network membership is enforced for TCP, presented for UDP.** A
  container connects over TCP to its own ports, its router and the egress
  proxy alone; its router carries a connection on only to a member it
  shares a network with. A connection to any other loopback port is
  refused (`ECONNREFUSED`), as in a network namespace where nothing
  listens there. Datagrams may go to any port of the stack's
  block, where routers' relays lie, so a program without the library
  reaches a member's UDP host port, and a host process that later binds a
  UDP port in the block is reachable from that stack.
- **Stacks meet through their routers.** On a shared network, and through
  legacy links, a datagram across stacks uses a relay port of the sender's
  router, idle-closed after 90 seconds. Without a link, containers on the
  default bridge reach one another only within a stack.
- **Internet access goes through a proxy.** Seatbelt cannot express "any
  host but this machine", so containers reach the internet through their
  stack's egress proxy: HTTP(S) via `HTTP_PROXY`/`HTTPS_PROXY`, any TCP via
  SOCKS5 (`ALL_PROXY`). UDP does not leave the machine. `fwsd -egress=false`
  turns the proxy off.
- **Ingress allow lists hold a container's published ports.** Its
  `fws.ingress.allow` label names the addresses and CIDR prefixes its
  published ports take connections and datagrams from; others are
  refused as they arrive. Docker has no such setting. Members of its
  stack and networks reach it by membership, not by this list.
- **Egress allow lists hold what goes through the egress proxy.** A
  container's `fws.egress.allow` label and its networks' option of that
  name (`docker network create --opt fws.egress.allow=...`) name the
  hosts, `*.domains`, addresses and prefixes, each with an optional
  `:port`, it may reach outside the machine; a destination must be on
  every list that applies. Docker has no such setting. A name is matched
  as the client asks for it, an address or prefix against every address
  it resolves to; UDP, which does not leave the machine, is not on them.
- **DNS is answered in the container and by its router.** The library
  resolves `/etc/hosts`, the stack's names and the host's resolver, through
  `getaddrinfo`, its async form, `DNSServiceGetAddrInfo` and
  Network.framework alike. A program that sends DNS itself (`dig`, Go's
  pure resolver) reaches the router, which answers the stack's names and
  passes other queries to the servers of the container's `resolv.conf`: a
  DNS server a member of the stack runs, over TCP at its port 53, and a
  server outside the machine given internet access. Of the options,
  `ndots`, `timeout` and `attempts` take effect. DNS settings are refused
  in a nested engine, as is a DNS server on loopback.
- **ICMP reaches the virtual network alone.** `ping` of loopback, the
  container or a member is answered by the library; an outside IPv4 host
  is pinged by the router given internet access. Raw sockets need root.
- **Published UDP clients are bounded by the stack's block**, one flow
  port each, idle-closed after 90 seconds. SCTP cannot be published.
- **Unix socket paths are capped at 104 bytes**, macOS's `sun_path`, as
  the container sees them; a longer host path binds through the mount or
  root below it. A program given a socket path should be given its
  container path, as `fws/postgres` and `fws/mysql` are
  (`/var/run/postgresql`, `/var/run/mysqld`): a host path under a stack's
  root often runs past the limit.

## Kubernetes

- **No kube-proxy or CoreDNS.** `fws-kube-proxy` serves Services and the
  library resolves cluster names.
- **A pod's limits hold, its requests set no priority.** The kubelet sends
  a container's memory and CPU limits, the pod's where it sets none, and
  the engine holds them as `--memory` and `--cpus`; requests only place
  pods, as a nice value would weigh a pod against every process on the
  Mac. `securityContext.runAsUser` takes no effect.
- **The CRI refuses checkpoints**; `UpdatePodSandboxResources` changes
  nothing, as containerd's does not. Pod and container metrics count CPU
  time in hundredths of a second, as `ps` reports it. A pod's DNS servers
  go unused; its search domains and options apply.
- **Pod networks hold the node's pod CIDR**, IPv4, IPv6 or both; other
  nodes' pods reach a pod through the routers. A changed CIDR applies once
  the node's pods are gone.
- **Services carry TCP and UDP**: cluster, external and load balancer IPs,
  node ports, `ExternalName` and `sessionAffinity: ClientIP` (loopback
  clients count as one). SCTP is not served. Changes arrive within the
  proxy's poll, a second by default.
- **kind runs on `fws/kind-node` only**, without systemd or containerd:
  `ctr` supports `images import`, `tag` and `ls`, and `journalctl` reads
  the node's logs. Of `containerdConfigPatches`, registry mirrors and a
  hosts directory apply; a node whose configuration sets anything else is
  not ready and names what it does not apply.
- **`fws/k3s` is kubeadm's cluster under k3s's names.** `k3s agent`,
  joining servers, Traefik, ServiceLB, the Helm controller, the embedded
  registry and etcd snapshots are absent. Its default storage class, `local-path`, is
  the controller manager's host-path provisioner. Its node needs an address
  outside loopback: the default bridge or a network with a subnet.

## Resources

Limits are enforced by the container's supervisor, `fws-shim`, every
100 ms, by the spawn helper and by the kernel's per-process limits, not by
a cgroup ([DESIGN](docs/DESIGN.md#3a-isolation-and-limits-without-cgroups)).

- **`--memory`** caps each process's footprint in the kernel at once
  (jetsam's fatal limit) and their sum in the shim; past either, the
  process, or the largest, is killed and the container reports
  `OOMKilled`. An allocation never fails first. A process forked without
  an exec is held by the sum alone. `--memory-swap` may only equal the
  limit (or be -1); `--memory-reservation`, swappiness and
  `--oom-kill-disable` are refused. A running container's limit can be
  lowered, not raised: the kernel lets no process raise its own.
- **`--cpus` and `--cpu-quota`** stop processes with `SIGSTOP` over quota,
  so a parent sees its children stop and continue. `--cpu-shares` sets the
  nice value, which only root lowers. Real-time scheduling is refused.
- **`--cpuset-cpus` holds a container to as many CPUs' time and, for
  efficiency cores, to them**, as macOS lets no process choose its CPUs.
  fws numbers the performance cores first, then the efficiency cores
  (`sysctl hw.perflevel0.logicalcpu`, `hw.perflevel1.logicalcpu`); a set of
  efficiency cores alone runs the container under the background QoS
  clamp, which macOS schedules on them, at background's priority and disk
  tier. Which cores within a level a process runs on is the kernel's
  choice. `--cpuset-mems` takes `0` alone, the Mac's one memory node. A
  running container's cpuset cannot change.
- **`--pids-limit`** counts processes, not threads: `fork` and
  `posix_spawn` fail with `EAGAIN` at the limit, and a process past it by
  a race or `vfork` is killed.
- **`--blkio-weight`** picks a disk I/O tier; an update reaches processes
  started after it. Per-device rates are refused.
- **`--ulimit`** sets nofile, fsize, cpu, core, stack, and memlock only
  unlimited; the rest are refused, as macOS counts nproc per user and
  enforces no data, as, rss or the others.
- **The engine guards the machine**: under critical memory pressure or
  under 2 GiB free disk it pauses the heaviest container, one every two
  seconds, until the pressure passes (`fwsd -machine-guard=false`).
  Containers share the user's process limit, held an eighth below
  `kern.maxprocperuid`, and run under a QoS clamp (`fwsd -qos-clamp`).
- **`--privileged` gives a container an engine of its own, nothing
  more**, for Docker in Docker (`fws/dind`) and kind nodes: no devices,
  capabilities or mounts. Linux `docker:dind`, `kindest/node` and Podman
  need a VM.
- **`docker pause` uses `SIGSTOP`**: connections to a paused container
  are accepted and wait.
- **Live restore needs `fwsd -live-restore`**; otherwise the engine stops
  its containers when it stops.
- **`docker stats` misses disk I/O a process does after the shim last
  sampled it**: the shim reads each process's counts when stats polls,
  each second, or every 100 ms with a limit set. Network I/O is the
  per-socket payload count on `eth0`, without headers.
- **Bandwidth is set by the `kubernetes.io/egress-bandwidth` and
  `kubernetes.io/ingress-bandwidth` labels**, or a pod's annotations of
  those names, in bits a second, as the Kubernetes bandwidth plugin takes
  them; Docker has no flag for it. They hold the TCP streams the
  container's router and its stack's egress proxy carry: every connection
  it makes to a member or the internet, both ways, and connections to its
  ports that have no host port. A connection to one of its host ports, and
  UDP, are not held.
- **`--storage-opt size` puts the root on a disk image of its own**, a
  sparse APFS image the engine attaches at the root, sized to the image's
  files and the quota beside them, so writes past it fail with `ENOSPC`.
  The image is copied into it, not cloned, so such a container is slower
  to create; the image's files are counted in whole APFS blocks, and a
  few megabytes of the volume's own are room beyond the quota. Volumes and
  tmpfs mounts stay outside it.
- **tmpfs mounts are directories**, emptied at each start, not
  memory-backed: `rw`, `ro`, `noexec`, `mode`, `uid` and `gid` hold, on
  tmpfs volumes too; `size` and `nr_inodes` are refused.
- **Local volumes bind host directories only** (`type=none,o=bind`) or are
  tmpfs; other filesystem types need root. A read-only bind source the
  host cannot create, such as `/lib/modules`, mounts empty.
- **Refused, having no macOS counterpart**: `--sysctl` beyond those below,
  runtimes other than runc, log drivers other than `json-file` and
  `local`, devices, `--gpus`, device cgroup rules, `--cgroup-parent`,
  `--oom-score-adj`, storage options other than `size`, Windows-only
  options, volume drivers other than `local`, and mount types other than
  bind, volume, image and tmpfs.
- **Accepted and changing nothing**, as each asks for no more confinement
  than a container has: `--cgroupns`, the sysctls that hold already
  (`net.ipv6.conf.{all,default}.disable_ipv6=0`, and in a privileged
  container IPv4 and IPv6 forwarding), and the capability and security
  options under [Isolation](#isolation). Bind propagation is always
  `rslave`.

## API surface

- **Testcontainers needs a few settings**: the libraries' reapers run as
  the catalog's `fws/ryuk` under the names each pins, Testcontainers for
  Java sees a blank `OSType` in `/info`, and its startup checks, which run
  `alpine`, are turned off ([README](README.md#testcontainers)).
- **A Podman pod shares its network, IPC and PID namespaces** through an
  infra container running `fws/pause`. `kube play` runs Pods, and
  Deployments of one replica, with their ConfigMaps' values, claims'
  volumes and init containers; `kube generate` writes a pod or container
  back and `kube down` removes what a file made. Checkpoints and quadlets
  are not served.
- **Not served:** swarm, services, nodes, secrets, configs and plugins.
  The engine serves what Docker Compose, the Docker CLI and Testcontainers
  use ([DESIGN](docs/DESIGN.md#8-docker-engine-api-surface)).
