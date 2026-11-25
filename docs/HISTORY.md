# History

What the project built, in the order it was built, and what the bugs along
the way taught. [`PLAN.md`](../PLAN.md) keeps what remains;
[`DESIGN.md`](DESIGN.md) describes the design as it stands. Figures are
measurements of their day, not current ones.

## The first milestone

Two independent Compose stacks (`postgres` + `redis` + a small app each)
run concurrently on a macOS CI runner, serve queries on different assigned
ports, cannot reach each other's ports or paths, and `down` on one leaves
the other working and frees its ports. Later pull requests widened the
Docker API, Compose and Testcontainers surface, each with its tests on the
macOS runner.

## Kubernetes in Docker (KinD)

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
   `kube-controller-manager`, `kube-scheduler`, `kubectl` and `kubeadm`,
   built with Nix from upstream's sources (`images/kubernetes`).
4. **A kubelet for macOS** (#50): upstream's kubelet with the darwin parts
   its tree lacks (`images/kubernetes/kubelet-darwin.patch`): machine and
   node statistics from macOS's kernel, a container manager without
   cgroups, and the static pod watcher on kqueue. fwsd serves CRI
   (`internal/cri`); pod sandboxes are `fws/pause` containers.
5. **Cluster networking and Services** (#53): pod addresses from the
   engine's IPAM, `fws-kube-proxy` mapping Services onto the engine,
   cluster names answered by the container library's resolver (no CoreDNS
   needed), and nested engines that share a network handing one another
   their pods.
6. **`fws/kind-node`** (#54): the node image kind expects, with kubeadm's
   phases run in place of systemd units, so `kind create cluster --image
   fws/kind-node` brings up single- and multi-node clusters.
7. **Tests**: `TestKindCluster` creates a two-node cluster, loads images,
   reaches a Deployment through its Service and NodePort, exports logs and
   deletes the cluster; `TestK3sModule` runs Testcontainers' k3s module on
   `fws/k3s`, built from `fws/kind-node`'s parts.

Bringing up the kubelet took, in order: quoting an
`--eviction-hard=memory.available<10Mi` the test's shell read as a
redirection; darwin implementations of Kubernetes' `hostutil`,
`mount-utils` and `subpath` packages (one mount namespace, so every mount is
in effect `rshared`; `getfsstat` for the mount table; a memory-backed
`emptyDir` is a directory on disk); `/etc/resolv.conf` written as Docker
writes it; and a CRI stream listener that opens on first use.

## Light, steady, general (#65 on)

The aim: containers lighter than a VM's, whose limits keep the machine
standing, with more of what Docker users write working, at no root, no VM
and no more than microseconds per call.

- **A faster test loop.** A round took about three hours. `images` now
  waits for the unit tests; the end-to-end suite runs in shards balanced by
  recorded durations; `fwsd -image-archives` loads an archive only when a
  test first asks for one of its images; `fws-mkimage -reuse` keeps each
  archive whose inputs are unchanged; and entry programs import small
  packages rather than the runtime, so a runtime change no longer rebuilds
  the catalog.
- **Limits the kernel enforces**: a QoS clamp on every container process,
  a fork ceiling below `kern.maxprocperuid`, and machine guards that pause
  the heaviest container under memory pressure or low disk.
- **Scale without cliffs**: event-driven process tracking, so an idle
  container costs nothing; clones and deletions outside the engine's lock.
  Port blocks that grow with a stack were not pursued: a profile cannot
  change once applied, so a running container could not reach a block
  added later.
- **Set aside**: mapping Linux package installs (`apt-get install`) to Nix,
  and a converter of Linux images. Dockerfiles stay in scope as far as
  macOS can run their steps, built `FROM` fws's base images (`PLAN.md`).

## Dev containers, GUI apps, DNS and IPv6 clusters (#67–#78)

- **Dev containers and GUI apps** (#67, #68, #77): the Dev Containers CLI,
  VS Code's server and VS Code itself run in containers, and
  `--security-opt seatbelt=gui` grants the login session's services.
  Programs signed with the hardened runtime are re-signed ad hoc so the
  container library loads into them. The engine writes `/etc/os-release`
  with `ID=darwin`, so VS Code downloads its macOS server, a zip piped to
  `tar -xz` that only libarchive's tar unpacks.
- **Memory limits in the kernel** (#71): CI's jetsam experiment showed that
  an unprivileged parent can set a child's fatal footprint limit with
  `posix_spawnattr_setjetsam_ext`, that it survives no fork or exec, and
  that no process can read or change it afterwards. Hence the limit set on
  every program a container starts, and no raising it by `docker update`.
- **What the library costs a start** (#72): `BenchmarkStartCost`, run in
  interleaved rounds since a runner's speed drifts more than the parts
  differ, put any inserted library at about 3.5 ms and `fwsroot`'s
  interposing at about 4 ms more, spread evenly over its functions, so no
  few are worth cutting.
- **DNS** (#72–#74): queries a program sends itself go to the router;
  Network.framework's private dnssd client is answered by the library,
  since a container reaches no mDNSResponder.
- **IPv6 clusters and kind's registry mirrors** (#75, #76).
- **No second sandbox** (#78): on macOS 15 and 26 a process under any
  Seatbelt profile, even `(allow default)`, is refused another, so Chromium
  keeps `--no-sandbox` (`TestSecondSandboxRefused`).

## The end of the roadmap and enforced isolation (#79–#85)

- **Presentation** (#79): the library presents `PATH` and `HOME` as the
  container's own, and `docker top` a command's paths. The roadmap was
  given an end, and version updates made upkeep.
- **The last gaps** (#80): the kubelet's pod limits, a stack's own DNS
  server, Podman's `kube play`.
- **Upkeep** (#81–#83): Kubernetes 1.37.2, etcd 3.7.4, kind 0.33.0 and the
  catalog at its newest releases, closer to upstream's defaults.
- **Enforced isolation** (#84): Seatbelt allow-lists a container's reads,
  as chroot would, instead of denying a list of host roots. A container
  connects over TCP only to its own ports, its router and the egress
  proxy, and the router carries a connection to another member only when
  the two share a network, so network membership is enforced rather than
  presented. The process table's listings are denied; the shim answers the
  library with the container's own processes.
- **Limits macOS can enforce** (#85): cpusets, a root size quota on a
  sparse disk image (hdiutil's `b` suffix is a 512-byte sector, which first
  made every quota 512 times its size), and bandwidth. A deadlock, Inspect
  taking a container's lock before the engine's, against Create's order,
  hung kind's clusters on `main`; a test now holds every function to the
  engine's lock first.

## Networks, images and two reviews (#86–#87)

- **#86**: egress and ingress allow lists; bridge options that hold, and
  refusal of those that do not; the container's own routes in the routing
  sysctl; Python, Node.js and Lambda images; `fws catalog`. A review of the
  whole codebase found security holes (a layer's hard link to its own
  symlink linked a host file into the root, since macOS `link(2)` follows
  a symlink source; whiteouts of `.` and `..`; clients without proxy
  credentials skipping allow lists), a nested-network deadlock, races of
  `docker update` with starts, and leaks. CI gained `scripts/lint.sh` and
  the C library's stricter warnings and clang's analyzer.
- **#87**: unparam, unconvert, deadcode and gosec joined the lint. gosec
  gates on a subset of its rules: its unchecked error (G104), integer
  conversion (G115), decompression (G110) and walk (G122) findings were
  read one by one and the two real conversions fixed; the rules left out
  flag what the engine exists to do, running and opening what containers
  ask for. nilaway was tried and fixed what it found, but its remaining
  reports are correlations it cannot follow, with no per-line suppression
  to gate on. Fixed besides: a `COPY` wildcard followed a context link to
  the host, and a git build context's ref could be a `git fetch` option.
  Testcontainers from Go, Java, Python, Node, Bun and Rust came under test,
  with these changes:
  - A mount's unowned files are the looking user's, not root's, so a
    non-root dev container user's Git accepts its workspace.
  - A process's `HOME` is its user's home from `/etc/passwd` where nothing
    names one.
  - A TCP connection to a loopback port outside the container's reach
    fails with `ECONNREFUSED`, as in a network namespace, not with
    Seatbelt's `EPERM`, which programs waiting for a server did not retry.
  - Testcontainers for Java refuses any engine whose OSType is not
    `linux`, but accepts a blank one, which `/info` now gives it.
  - `SetServices` closed old ports before binding new ones, so a node port
    another process held left a network half updated; it now changes
    everything or nothing.
  - `fws-mkimage` refuses an image path in a tree the library leaves to
    the host, where no container would see it.

## Investigations

Findings still worth knowing, each with what the code does since.

**Processes outliving their container.** PostgreSQL's `pg_ctl` starts the
server in a session of its own, and the server overwrites its arguments and
environment, so neither the process group nor an environment tag found its
children. The kernel's lasting mark is the *responsible process*: the main
process is spawned with responsibility disclaimed, so macOS holds it
responsible for everything started under it, and the shim follows those
processes.

**ClickHouse's exit code 134.** ClickHouse listens on `::` and then
`0.0.0.0` at one port, which the library binds to one socket file; the
library now refuses the second bind with `EADDRINUSE`, as Linux does.
Darwin fails `getpeername` and `setsockopt` with `EINVAL` on a socket shut
down both ways, where Linux succeeds, so the library sets
`SONPX_SETOPTSHUT` on every listener. And `pthread_setname_np` names a
thread through `proc_info` of `getpid()`, which the library made 1, the
kernel's launchd; `getpid` returns the host pid in that call.

**Create raced Testcontainers' reaper.** Cloning a large root under the
engine's lock took 10 s, long enough for the previous subtest's reaper to
remove the new container. Create clones before taking the lock, and
removal moves a root to a trash directory.

**Links in an image resolved on the host.** Mount points, hosts files, the
home directory, a build's `COPY` and volume copy-up followed absolute links
to host paths, so `/data -> /Users/me` with a volume at `/data/x` wrote the
host's `/Users/me/x`. Each now resolves inside the root, as a chroot would.

**`docker compose up --build` stalls.** BuildKit's snapshotter measured
each committed snapshot by walking every file of all layers below, 6 to 7 s
per layer of a Nix image. It now measures only what a snapshot adds, when
BuildKit first asks. CRI's `ImageFsInfo` likewise walked every root on each
call; roots are now measured once at unpack, container roots at most every
30 s.

**Per-call overhead.** Each `stat` read two extended attributes at about
9 µs apiece; the library now reads them only from files marked
`UF_NODUMP`, which `stat` returns anyway, and tries a call on the
container's file first with the kernel refusing links on the way. `stat`
and `open` of a file the container holds run within about a microsecond of
native.

**Container start cost.** A start took 1.15 s because the suite built
`fws-shim` with the race detector, and a race-built Go binary takes about
half a second to start on macOS. Of the remaining ~20 ms, compiling the
profile took about 15 ms while applying bytecode takes 35 µs, so profiles
are compiled once and kept. The Go spawn helper gave way to the C helper,
which forks the exec anchor itself.

**MongoDB's `ECONNREFUSED 127.0.0.1:65535`.** A router request began with
the port, and `0xffff` marked a relay, so `mongosh`'s SOCKS5 tunnel on an
ephemeral port 65535 was refused, one run in 16384. Requests now lead with
a byte naming their kind.

**The pids limit's cost.** Every `fork` listed every process of the user to
count the container's. The shim now reads a fork's children from the
kernel's per-parent list and answers the library's count on a socket.
