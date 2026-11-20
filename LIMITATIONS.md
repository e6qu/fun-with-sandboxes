# Limitations

Where the engine differs from Docker on Linux, and why. Almost every limit
follows from the two constraints, no root and no virtual machine, and from
what macOS offers an unprivileged process under them. Non-goals are in
[PLAN.md](PLAN.md#non-goals).

## Images

- **macOS-native images only.** Nothing on macOS runs Linux ELF binaries
  without a VM, so stock Docker Hub images do not run.
- **Images follow the container contract.** A service finds its root and
  ports at their container paths and port numbers through the container
  library, or reads them from the `SANDBOX_*` variables (README). Software
  the library cannot load into needs an entrypoint that configures it from
  the variables.
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
- **Catalog images are built by Nix**, mostly from source; MongoDB and
  Vault are the vendors' own macOS builds. Where Linux parts are missing
  they differ from upstream's:
  - `fws/kafka` is Apache Kafka in Confluent's layout, without Confluent's
    own components such as the REST Proxy; `KAFKA_TOOLS_LOG4J_LOGLEVEL`
    sets nothing.
  - `fws/meilisearch` serves no mini-dashboard and has no `meilitool`.
  - `fws/busybox` holds separate GNU and other darwin programs, not
    BusyBox's applets, so options differ; its `tar` is libarchive's, as
    macOS's is; its `nc` only checks a port (`nc -z`); it has no `ps`,
    `top` or `vi`. `fws/base` has no package manager: add programs with
    `COPY` or by downloading darwin builds.
  - `fws/qdrant` serves the older web UI nixpkgs builds.
  - `fws/neo4j` is the Community Edition.
  - `fws/opensearch` is the minimal distribution, without the security,
    k-NN and ML Commons plugins, so `DISABLE_SECURITY_PLUGIN` and the demo
    configuration act on nothing.
  - Kubernetes' images carry `/bin/sh` and `/bin/nc`, where upstream's are
    distroless; there is no kube-proxy or CoreDNS image.
- **BuildKit builds on one darwin worker.** Multi-platform builds and
  `# syntax=` frontends other than Docker's own are refused (gateway
  frontends ship as Linux images); `# syntax=docker/dockerfile:<version>`
  uses the vendored built-in frontend. An SSH mount's socket belongs to the
  engine's user whatever `uid` it names.
- **Build steps run under Seatbelt**, with internet through a build-only
  egress proxy (none with `--network none`).

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
    any other path, such as `/dev` or the system frameworks, names the
    host's. `/proc` is an empty directory. Symbolic links resolve inside
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
  denies signalling or inspecting any other. Apple's `ps` sees host pids
  and every process of the user.
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
  ([DESIGN](docs/DESIGN.md#accepted-exceptions)).

## Networking

- **No host network mode**, except inside a privileged container's nested
  engine: every container is confined to its stack's ports.
- **Bridge networks and the default IPAM driver only.** Overlay, macvlan,
  ipvlan, other IPAM drivers and IPAM options are refused.
- **Container addresses are presented, not routed.** Members have
  addresses in `127.88.0.0/16`, or in their network's subnets (the default
  bridge holds `172.17.0.0/16`, where `--ip` is refused, as in Docker);
  networks may be IPv4, IPv6-only or dual-stack. The kernel never sees
  these addresses:
  - Servers see clients connect from `127.0.0.1`; a UDP server sees an
    IPv6 client at its IPv4-mapped address.
  - A program without the library resolves no member name; it uses
    `$SANDBOX_<SERVICE>_HOST:$SANDBOX_<SERVICE>_PORT_<n>`.
  - A container keeps its addresses while it exists. There is no
    link-local IPv6 address, and a user-set MAC address is refused.
  - A network without a subnet reports `127.88.0.0/16`, so a program that
    advertises its first global address (Kubernetes' API server,
    SeaweedFS) finds none: give it a subnet.
- **Interfaces are presented.** `getifaddrs`, the `SIOCGIF*` ioctls (IPv4)
  and the routing sysctl list loopback and `eth0`; binding to `eth0`'s
  index or joining a multicast group on it fails.
- **A container's ports are its loopback host ports.** Only exposed or
  published ports have host ports; a listener on any other port, or port
  0, listens on a Unix socket reached through the router. A dual-stack
  listener listens on `127.0.0.1` alone. A loopback-only TCP listener is
  unpublished, as in Docker, but stack members still reach it. Programs
  without the library see the host ports.
- **A stack's block is one reachability domain.** Within a stack, network
  membership is presented, not enforced: a program without the library
  reaches any member's host port. A host process that later binds a port
  in a stack's block is reachable from that stack.
- **Stacks meet through their routers.** On a shared network, and through
  legacy links, a datagram across stacks uses a relay port of the sender's
  router, idle-closed after 90 seconds. Without a link, containers on the
  default bridge reach one another only within a stack.
- **Internet access goes through a proxy.** Seatbelt cannot express "any
  host but this machine", so containers reach the internet through their
  stack's egress proxy: HTTP(S) via `HTTP_PROXY`/`HTTPS_PROXY`, any TCP via
  SOCKS5 (`ALL_PROXY`). UDP does not leave the machine. `fwsd -egress=false`
  turns the proxy off.
- **DNS is answered in the container and by its router.** The library
  resolves `/etc/hosts`, the stack's names and the host's resolver, through
  `getaddrinfo`, its async form, `DNSServiceGetAddrInfo` and
  Network.framework alike. A program that sends DNS itself (`dig`, Go's
  pure resolver) reaches the router, which answers the stack's names and,
  given internet access, passes other queries to the servers of the
  container's `resolv.conf`. `--dns`, `--dns-search` and `--dns-option`
  need internet access; of the options, `ndots`, `timeout` and `attempts`
  take effect. A DNS server inside a stack, or a Service, cannot serve.
  DNS settings are refused in a nested engine, as is a DNS server on
  loopback.
- **ICMP reaches the virtual network alone.** `ping` of loopback, the
  container or a member is answered by the library; an outside IPv4 host
  is pinged by the router given internet access. Raw sockets need root.
- **Published UDP clients are bounded by the stack's block**, one flow
  port each, idle-closed after 90 seconds. SCTP cannot be published.
- **Unix socket paths are capped at 104 bytes**, as the container sees
  them; a longer host path binds through the mount or root below it.

## Kubernetes

- **No kube-proxy or CoreDNS.** `fws-kube-proxy` serves Services and the
  library resolves cluster names.
- **The kubelet applies no Linux settings**, so `resources.limits` and
  `securityContext.runAsUser` take no effect.
- **The CRI refuses checkpoints**; `UpdatePodSandboxResources` changes
  nothing, as containerd's does not. Pod metrics count CPU time in whole
  seconds. A pod's DNS servers go unused; its search domains and options
  apply.
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
  registry and etcd snapshots are absent. Its node needs an address
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
  nice value, which only root lowers. Cpusets and real-time scheduling are
  refused.
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
- **`docker stats` samples the host's `ps` each second**, so a process
  that lives less misses its disk I/O; network I/O is the per-socket
  payload count on `eth0`, without headers.
- **tmpfs mounts are directories**, emptied at each start, not
  memory-backed: `rw`, `ro`, `noexec`, `mode`, `uid` and `gid` hold, on
  tmpfs volumes too; `size` and `nr_inodes` are refused.
- **Local volumes bind host directories only** (`type=none,o=bind`) or are
  tmpfs; other filesystem types need root. A read-only bind source the
  host cannot create, such as `/lib/modules`, mounts empty.
- **A Podman pod shares its network, IPC and PID namespaces** through an
  infra container running `fws/pause`. `generate kube`, `play kube`,
  checkpoints and quadlets are not served.
- **Refused, having no macOS counterpart**: `--sysctl` beyond those below,
  `--userns`, runtimes other than runc, log drivers other than `json-file`
  and `local`, devices, `--gpus`, device cgroup rules, `--cgroup-parent`,
  `--oom-score-adj`, `--storage-opt`, Windows-only options, volume drivers
  other than `local`, and mount types other than bind, volume, image and
  tmpfs.
- **Accepted and changing nothing**, as each asks for no more confinement
  than a container has: `--cap-drop`, `seccomp`/`apparmor=unconfined`,
  `label=disable`, `--cgroupns`, and the sysctls that hold already
  (`net.ipv6.conf.{all,default}.disable_ipv6=0`, and in a privileged
  container IPv4 and IPv6 forwarding). Bind propagation is always
  `rslave`.

## API surface

- **Not served:** swarm, services, nodes, secrets, configs, plugins and
  image search. The engine serves what Docker Compose, the Docker CLI and
  Testcontainers use ([DESIGN](docs/DESIGN.md#8-docker-engine-api-surface)).
