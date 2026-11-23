# History

What the project built, in the order it was built, and the investigations
behind the bugs it fixed. [`PLAN.md`](../PLAN.md) keeps what remains; this
file keeps how things came to be, so a later change can learn why. Figures
are measurements of their day, not current ones.

## The first milestone

Two independent Compose stacks (`postgres` + `redis` + a small app each)
run concurrently on a macOS CI runner, serve queries on different assigned
ports, cannot reach each other's ports or paths, and `down` on one leaves
the other working and frees its ports. Later pull requests widened the
Docker API, Compose and Testcontainers surface, each with its tests on the
macOS runner.

## Kubernetes in Docker (KinD), steps 1–7

`kind create cluster` against fwsd: kind starts each node as a privileged
container from a node image, and the node runs a control plane and a
kubelet whose pods are containers. The official `kindest/node` image is
Linux, so the project built the same contract from macOS parts:

1. **Nested engines and `fws/dind`** (#48): a privileged container gets an
   engine of its own, whose containers run in sandboxes the outer engine
   applies.
2. **Podman's API** (#48): kind's `podman` provider and `podman-compose`
   drive fwsd as its `docker` provider does.
3. **A control plane for darwin** (#50): `etcd`, `kube-apiserver`,
   `kube-controller-manager`, `kube-scheduler`, `kubectl` and `kubeadm`
   1.35.0, the release kind 0.31's node image ships, built with Nix from
   upstream's sources (`images/kubernetes`).
4. **A kubelet for macOS** (#50): upstream's kubelet with the darwin parts
   its tree lacks (`images/kubernetes/kubelet-darwin.patch`): machine and
   node statistics from macOS's kernel, a container manager without
   cgroups, and the static pod watcher on kqueue. fwsd serves CRI on its
   sockets (`internal/cri`); pod sandboxes are `fws/pause` containers, and
   the stream server on port 10010 serves exec, attach and port-forward.
5. **Cluster networking and Services** (#53): pod addresses from the
   engine's IPAM on a per-node pod network, `fws-kube-proxy` mapping
   Service ClusterIPs and NodePorts onto the engine's routers, cluster
   names answered by the container library's resolver (no CoreDNS
   needed), and nested engines that share a network handing one another
   their pods, so pods reach other nodes' pods.
6. **`fws/kind-node`** (#54): the node image kind expects, with kubeadm's
   phases run against the components above in place of systemd units, so
   `kind create cluster --image fws/kind-node` brings up single- and
   multi-node clusters.
7. **Tests**: `TestKindCluster` creates a two-node cluster, loads images,
   reaches a Deployment through its Service and NodePort, exports logs and
   deletes the cluster; Testcontainers' k3s module runs `fws/k3s`, built
   from `fws/kind-node`'s parts (`TestK3sModule`).

## Light, steady, general: the track as of November 2026

The aim from #65 on: containers lighter than a VM's, whose limits keep the
machine standing, with more of what Docker users write working, at no root,
no VM and no more than microseconds per call.

1. **A faster test loop.** A round took about three hours: `images` built
   the catalog for 60 to 90 minutes, then `e2e` ran for 50. Now `images`
   waits for the unit tests; the end-to-end suite runs in four shards
   balanced by recorded durations; `fwsd -image-archives` loads an archive
   only when a test first asks for one of its images; and `fws-mkimage
   -reuse` keeps each archive whose inputs' digest (`<archive>.inputs`) is
   unchanged, so a change to one image rebuilds that one. Entry programs
   import small packages (`internal/execpath`) rather than the runtime, so
   a runtime change no longer rebuilds the catalog.
2. **Limits that keep the machine standing**, enforced by the kernel where
   an unprivileged process can ask it to:
   - A QoS clamp on every container process (`fwsd -qos-clamp`, `utility`
     by default), set through `posix_spawn`'s `POSIX_SPAWN_SETEXEC`, the
     one exec that takes it.
   - A fork ceiling: `RLIMIT_NPROC`, which macOS counts across the user's
     processes, an eighth below `kern.maxprocperuid`.
   - Machine guards (`-machine-guard`): under critical memory pressure or
     under 2 GiB of free disk, the engine pauses the container using the
     most, one at a time, and resumes them once pressure is normal and
     4 GiB are free.
3. **Scale without cliffs.**
   - Event-driven process tracking: a supervisor follows its container's
     processes by the kernel's fork, exec and exit events and reads a
     fork's children with `proc_listpids(PROC_PPID_ONLY)`, so an idle
     container costs nothing.
   - Less under the engine's lock: a fresh volume's copy-up is an APFS
     clone, a stack's network file is written only when it changes, and
     removed containers and volumes go to a trash directory emptied
     outside the lock.
   - Port blocks that grow with a stack were not pursued: a profile cannot
     change once applied, so a running container could not reach ports in
     a block added later. The block size stays configurable
     (`-block-size`).
   - Process starts: the spawn helper became a C program that starts the
     exec anchor itself (see the investigations below).
4. **Fewer correctness gaps**: UDP Services, served by the engine and
   relayed by the container's router; UDP between pods of different nodes,
   sent from the destination stack's port block; ICMP echo on the virtual
   network, answered locally or sent by the router through the
   unprivileged ICMP socket; and `# syntax=docker/dockerfile:*` served by
   the built-in frontend
   (`third_party/buildkit-patches/syntax_builtin.patch`).
5. **Set aside**: mapping Linux package installs (`apt-get install`) to
   Nix, and a converter of Linux images. Dockerfiles stay in scope as far
   as macOS can run their steps, built `FROM` fws's base images; Linux
   binaries and Linux-specific Dockerfiles are not (`PLAN.md`).

## Dev containers, GUI apps, DNS and IPv6 clusters (PRs #67–#78)

- **Dev containers and GUI apps** (#67, #68): the Dev Containers CLI
  brings up `devcontainer.json` on darwin images, VS Code's server and
  VS Code itself (extension host, file events, terminal) run in a
  container, and `--security-opt seatbelt=gui` grants a container the
  login session's services for graphical apps. Programs signed with the
  hardened runtime are re-signed ad hoc so the container library loads
  into them (`internal/pathmap/src/hardened.h`).
- **Smaller fixes** (#69, #70): an attach's input reaches the container
  only after the output stored before the attach, so the answer to it is
  delivered; `posix_spawn` file actions (a child's working directory and
  opened files, as libuv uses them) name the container's paths. #70 added
  the VS Code Dev Containers extension's test, CRI events and metrics, pod
  DNS, asynchronous lookups (`getaddrinfo_async_start`,
  `DNSServiceGetAddrInfo`) and a fuzzer of the path mapping under ASan and
  UBSan.
- **Memory limits in the kernel** (#71): CI's jetsam experiment showed that
  an unprivileged parent can set a child's fatal footprint limit with
  `posix_spawnattr_setjetsam_ext`, that the limit survives no fork or exec,
  and that no process can read or change it afterwards. The spawn helper,
  the container library and `fws-memexec` set the container's limit on
  every program a container starts; the shim counts kills at the limit
  (`NOTE_EXIT_MEMORY`) as OOM kills; `docker update` refuses to raise a
  running container's limit.
- **What the library costs a start** (#72): `BenchmarkStartCost`, in
  interleaved rounds since a runner's speed drifts more than the parts
  differ, put any inserted library at about 3.5 ms and `fwsroot`'s
  interposing at about 4 ms more, spread evenly over its 160 functions, so
  no few are worth cutting.
- **DNS through the router** (#72, #73): a program that sends DNS itself
  (`dig`, Go's pure resolver) has its queries to outside servers sent to
  the container's router, which answers the stack's names and forwards the
  rest to the container's `resolv.conf` servers when it has internet
  access. TCP and IPv6 servers go the same way.
- **Network.framework** (#74): a container reaches no mDNSResponder, so the
  library stands in for the private dnssd client, resolving names as its
  `getaddrinfo` does and handing back results the client's getters read;
  `connectx` is routed as `connect` is.
- **IPv6 clusters** (#75): nested engines publish members' IPv6 addresses
  and which networks are IPv6-only, so pods on IPv6-only networks reach
  other nodes, and the CRI's pod network takes every subnet of the pod
  CIDR: kind's IPv6 and dual-stack clusters run.
- **kind's `containerdConfigPatches`** (#76): the node writes containerd's
  configuration and the CRI pulls through the registry mirrors and
  `hosts.toml` hosts it names, as kind's local registry setup needs; any
  other setting it cannot apply leaves the node not ready, naming it.
- **VS Code's own server install** (#77): the engine writes
  `/etc/os-release` with `ID=darwin` into a container whose image has
  none, so the Dev Containers extension downloads VS Code's macOS server
  itself. That server is a zip piped to `tar -xz`, which macOS's bsdtar
  unpacks and GNU tar does not, so `fws/busybox` and `fws/base` carry
  libarchive's tar.
- **No second sandbox** (#78): on macOS 15 and 26, a process under any
  Seatbelt profile, even `(allow default)`, is refused another with
  `EPERM`, through `sandbox_init` and `sandbox_apply` alike. Chromium
  therefore keeps `--no-sandbox` in a container; `TestSecondSandboxRefused`
  checks this. The catalog moved to nixpkgs-26.05-darwin of 2026-11-19.

## To the end of the roadmap (PRs #79–#80)

- **Presentation** (#79): the library presents `PATH` and `HOME` as the
  container's own and hands programs it starts the root's; `docker top`
  reads a command's host paths as the container's; tmpfs mounts and
  volumes take `uid` and `gid` as recorded owners. The documents were cut
  to what is true, the roadmap given an end, and version updates made
  upkeep.
- **The last gaps** (#80): the darwin kubelet sends a container's memory
  and CPU limits, which the CRI holds as `--memory` and `--cpus`; the
  router asks a DNS server a member of the stack runs, over TCP at its
  port 53; and Podman's `kube play`, `kube generate` and `kube down` run,
  write back and remove Pods and Deployments.

## Upkeep and enforced isolation (PRs #81–#84)

- **Newest releases** (#81, #82): Kubernetes 1.37.2, etcd 3.7.4 and kind
  0.33.0, and every catalog image at its newest upstream release. The
  library now shows a script the path it was run by, which fixed Kafka's
  tool wrappers.
- **Closer to upstream** (#83): Meilisearch now ships with its default
  features, including the Lindera dictionaries. OpenSearch now ships with
  its security plugin. CI's cache was moved within its 10 GB by building
  Kubernetes and Consul in prebuilt groups.
- **Enforced isolation** (#84): Seatbelt allow-lists a container's reads,
  as chroot would, instead of denying a list of host roots. It lets a
  container connect over TCP only to its own ports, its router and the
  egress proxy. The router carries each connection to another member only
  when the two share a network, so network membership is enforced. The
  process table's listings are denied as well; the container's supervisor
  answers the library with the container's own processes.

## Limits macOS can enforce (PR #85)

- **Cpusets**: a set holds a container to as many CPUs' time, and a set
  of efficiency cores to them through the background QoS clamp.
- **A root size quota**: `--storage-opt size` puts the root on a sparse
  APFS disk image of its own; hdiutil's `b` suffix is a 512-byte sector,
  which first made every quota 512 times its size.
- **Bandwidth**: the Kubernetes bandwidth labels and annotations hold a
  container's TCP through its router and the egress proxy.
- **A deadlock**: Inspect took a container's lock and then the engine's,
  against Create's order, which hung kind's clusters on main; a test now
  holds every function to the engine's lock first.

## Networks, images and the CLI (PR #86)

- **Allow lists**: `fws.egress.allow` on a network or container holds the
  egress proxy to the hosts, addresses and ports it names, refused with
  403; `fws.ingress.allow` holds a published port to the source addresses
  it names.
- **Bridge options**: `enable_icc`, `enable_ip_masquerade` and
  `host_binding_ipv4` hold; options the engine took and dropped before
  are refused.
- **Routes**: the routing sysctl (`netstat -rn`) showed the host's routes
  to every container; the library now presents the container's own, its
  subnets and a default route through its network's gateway.
- **Images**: Python 3.11–3.15, Node.js 22–26 and their Lambda runtimes;
  `fws catalog` lists the catalog, and CI checks each tag it lists
  publishes. `fws-mkimage -env` replaced none of a spec's variables, so
  an image could carry two of one name.

## Investigations

Findings still worth knowing, each with what the code does since.

**Processes outliving their container.** PostgreSQL's `pg_ctl` starts the
server in a session of its own, and the server overwrites its arguments
and environment, so neither the process group nor an environment tag
found its children. The kernel's lasting mark is the *responsible
process*: the main process is spawned with responsibility disclaimed, so
macOS holds it responsible for everything started under it. The shim
follows those processes while the main process lives and kills them after
the group; they count against the container's limits and appear in `top`
and `stats`.

**ClickHouse's exit code 134.** ClickHouse listens on `::` and then
`0.0.0.0` at one port, which the library binds to one socket file; the
second bind probed the first and left an acceptor stuck. The library now
refuses that bind with `EADDRINUSE`, as Linux does. Darwin also fails
`getpeername` and `setsockopt` with `EINVAL` on a socket shut down both
ways, where Linux succeeds: the library names the client the router
recorded and sets `SONPX_SETOPTSHUT` on every listener. The last cause:
`pthread_setname_np` names a thread through `proc_info` of `getpid()`,
which the library makes 1 in the main process, so the kernel refused to
name a thread of launchd; `getpid` returns the host pid in that call.

**Bringing up the kubelet.** In order: the test's shell read an unquoted
`--eviction-hard=memory.available<10Mi` as a redirection; Kubernetes'
`hostutil`, `mount-utils` and `subpath` packages have no darwin
implementation, which the kubelet patch adds (one mount namespace, so
every mount is in effect `rshared`; `getfsstat`/`statfs` for the mount
table; a memory-backed `emptyDir` is a directory on disk; subpaths without
bind mounts); the engine writes `/etc/resolv.conf` as Docker does, and the
test's kubelet reads `/dev/null` as its own so pods get no nameservers
the engine would refuse; the CRI stream listener opens on first use, after
the shim makes the port directory; and the engine's Go listeners strip the
client-address prefix the router writes before each stream, as the shim's
proxy for a nested engine's published ports does.

**Create raced Testcontainers' reaper.** Cloning mongosh's large root twice
under the engine's lock took 10 s, long enough for the previous subtest's
reaper to remove the new container. Create now clones before taking the
lock.

**Links in an image resolved on the host.** Mount points, hosts files,
the home directory, a build's `COPY` and volume copy-up followed absolute
links to host paths, so `/data -> /Users/me` with a volume at `/data/x`
wrote the host's `/Users/me/x`. Each now resolves inside the root, as a
chroot would.

**`docker compose up --build` stalls.** BuildKit's sessions failing their
health checks were no cause; they behave so under dockerd too. The stall
was the snapshotter measuring each committed snapshot by walking every
file of all layers below, 6 to 7 s per layer of a Nix image. It now
measures only what a snapshot adds, when BuildKit first asks.

**Per-call overhead.** `go test -bench . ./internal/pathmap` compares each
interposed call with the native one. Each `stat` read two extended
attributes (owner, mode) at about 9 µs apiece; the library now reads them
only from files marked `UF_NODUMP`, which `stat` returns anyway. `stat`,
`lstat`, `fstatat` and `open` without `O_CREAT` first try the call on the
container's file with the kernel refusing links on the way, so a file the
container holds costs no lookup. With these, `stat` and `open` of a file
the container holds run within about a microsecond of native.

**Engine endpoint times.** `-debug` reports time per endpoint over the
suite. Removal deleted a root of as many files as its image under the
engine's lock, and create cloned under it; create now clones into staging
before the lock and removal moves to a trash directory. CRI's
`ImageFsInfo` walked every root on each call (66 s on the first); image
roots are now measured once at unpack, container roots in the background
at most every 30 s.

**Container start cost.** A start took 1.15 s because the suite built
`fws-shim` with the race detector, and a race-built Go binary takes about
half a second to start on macOS; only fwsd is race-built now. Of the
remaining ~20 ms, compiling the profile took about 15 ms while applying
bytecode takes the kernel 35 µs: profiles are compiled once and kept
(`profile.bin`). The Go spawn helper and anchor launcher gave way to the C
helper `internal/pathmap/src/spawn.c`, which forks the anchor itself. A
start then took about 5.6 ms against 1.7 ms native, the rest being the
loader's cost for the inserted library (#72 above). The reaper's 3.8 s
first start was clang building the library and helper; `fwsd` builds them
as it starts.

**MongoDB's `ECONNREFUSED 127.0.0.1:65535`.** `mongosh` honours the egress
proxy variables by opening a SOCKS5 tunnel on port 0, which the library
binds to a port from 49152–65535. A router request began with the port,
and `0xffff` marked a relay to another stack, so port 65535 was refused,
one run in 16384. Requests now lead with a byte naming their kind.

**The pids limit's cost.** Every `fork` listed every process of the user to
count the container's. The supervisor now reads a fork's children from the
kernel's per-parent list and answers the library's count on a socket, as
`pids.current` would; a test of twelve forks holds full table reads to
three. A launcher that forks its daemon and ends before it is watched
makes the supervisor read the table again.
