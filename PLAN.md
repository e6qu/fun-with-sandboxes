# fun-with-sandboxes — plan

A Docker-API-compatible container engine for macOS that runs Compose stacks
side by side on one Mac, each isolated from the host and from the others,
built only on primitives macOS gives an unprivileged user.

Detailed design: [`docs/DESIGN.md`](docs/DESIGN.md). This file is the
top-level plan and milestone; the design doc is the full mechanism.

## Constraints

- **No virtual machines.** No Hypervisor.framework, Virtualization.framework,
  or Apple's Containerization framework.
- **No root.** The engine, its install, and every container run as the
  invoking user. No `sudo`, no `launchd` daemon running as root, no kernel or
  system extensions, no changes to System Integrity Protection.
- **Go.** Every component is Go and cross-compiles with `CGO_ENABLED=0`
  (`GOOS=darwin`, `GOARCH=arm64|amd64`) from any host.
- **Real behaviour.** Every isolation property claimed below is enforced by
  the kernel through Seatbelt and is proven by a test that tries to break it.

## Non-goals

- Running Linux images. Without a VM nothing on macOS executes Linux ELF
  binaries, so images are macOS-native (`darwin/arm64`, `darwin/amd64`).
- A security boundary against hostile code. Containers run as the same user as
  the engine; Seatbelt confines them, but it is defence for development and
  test workloads, not multi-tenant isolation.
- Full Docker API coverage. The engine serves the subset Docker Compose, the
  Docker CLI, and Testcontainers use (listed in the design doc).
- Multi-platform builds. BuildKit builds darwin images on one worker; images
  also come from the Nix image pipeline through `docker load` or a registry
  pull.

## Core idea

macOS gives an unprivileged process no private `/` and no private network
stack, so isolation is **by assignment, enforced by Seatbelt** — not by
virtualization. The engine is the authority that hands each stack a disjoint
port range and path tree; Seatbelt is the fence that stops a container using
anything it was not handed. Services take their ports and data paths from
configuration, which is how per-stack isolation is expressed without a private
mount or network namespace.

## Isolation model

| Resource | Mechanism | Guarantee | Accepted exception |
|---|---|---|---|
| Filesystem writes | APFS `clonefile` root per container; Seatbelt `file-write*` allowed only on the container root, its volumes and mounts, and `/dev/null`-style devices | A container writes nowhere else | — |
| Host reads | Seatbelt deny-default profile: `file-read*` allowed on the container root, its volumes, and the system paths every macOS process needs (`/System`, `/usr/lib`, `/usr/share`, `/bin/sh`, `/dev`) | A container reads its own files and the OS, nothing of the user's | The macOS system tree is readable |
| Container view | The container library, `fwsroot`, loaded into every container process, maps paths into the root, presents the container's user, groups, hostname and pids | Programs see `/`, `--user`, `--hostname` and pid 1 as under Docker | Programs Apple signs, and static ones, load no library |
| Cross-stack visibility | Per-stack path tree; a stack's Seatbelt profile names only its own paths and ports | Stack A cannot read, write, or connect to stack B | — |
| Processes | Seatbelt `signal` and `process-info*` restricted to the same sandbox; the library lists a container's own processes alone | A container cannot signal or inspect another's processes | Processes keep their host pids beyond pid 1 |
| Ports | Engine allocates a disjoint host-port block per stack; Seatbelt `network-bind`/`network-outbound` allowed only on the stack's ports; ports without host ports go through a per-container router over Unix sockets | Two stacks never clash; a container binds only its grants | Host sees all listening sockets globally |
| Resource limits | Each container's supervisor, `fws-shim`, enforces `--memory`, `--cpus`, `--pids-limit` and the block I/O tier; `setrlimit` for `--ulimit` | Limits hold to within one 100 ms sample | Knobs only the kernel could honour are refused |
| Identity | Runs as the invoking user; the library presents the container's user from its own `/etc/passwd` | `--user` and `USER` behave as in Docker | Presented, not enforced |

Rejected knobs return a precise API error; silent no-ops would be a fidelity
bug. `LIMITATIONS.md` lists every one.

## Components (Go)

```
cmd/fwsd/             # the daemon: Docker Engine API over a unix socket
cmd/fws-shim/         # the helper binary: spawn helper, supervisors, egress proxies
cmd/fws-mkimage/      # Nix closure -> darwin image archive
internal/alloc/       # per-stack port blocks and directories
internal/seatbelt/    # Seatbelt profile rendering
internal/image/       # image store, layer extraction, clonefile roots
internal/runtime/     # launch under sandbox-exec, process groups, pipes
internal/shim/        # a container's supervisor: limits, output, live restore
internal/roles/       # the helper processes the engine's binary runs
internal/libsystem/   # libSystem calls syscall does not offer (posix_spawn)
internal/pathmap/     # fwsroot, the container library, and its build
internal/identity/    # users and groups from the container's /etc/passwd
internal/members/     # a stack's network file: member addresses and names
internal/router/      # traffic to container ports without host ports
internal/portproxy/   # published host ports, TCP and UDP
internal/egress/      # the per-stack HTTP and SOCKS5 proxy to the internet
internal/netstat/     # per-socket network counters for docker stats
internal/procs/       # the host process table, for top and stats
internal/pidns/       # PID namespaces containers share, through their shims
internal/logfile/     # json-file logs
internal/dockerfile/  # Dockerfile parsing and variable expansion
internal/buildkit/    # embedded BuildKit with a darwin worker
third_party/buildkit/ # the BuildKit packages it uses, patched (sync-buildkit.sh)
internal/rootfs/      # chroot-style path resolution inside a root
internal/engine/      # containers, volumes, networks, exec, health, events
internal/api/         # the Docker Engine API (the Compose subset)
internal/sandboxenv/  # the container side of the SANDBOX_* contract
images/               # entrypoints and specs for the darwin catalog
tests/e2e/            # Docker CLI, Compose and Testcontainers end-to-end suite
```

## Image catalog

A Nix-built catalog for `aarch64-darwin`/`x86_64-darwin`, converted to OCI
layers with a `darwin/<arch>` platform entry: `bun`, `clickhouse`, `consul`, `dind`, `dynamodb`,
`etcd`, `gitea`, `influxdb`, `kafka`, `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, `kubectl`, `kubelet`, `lambda`, `mailpit`, `mariadb`, `meilisearch`, `memcached`, `mongo`, `mysql`, `nats`,
`neo4j`, `nginx`, `node`, `opensearch`, `pause`, `playwright`, `postgres`, `python`, `qdrant`, `rabbitmq`, `redis`,
`ryuk` (Testcontainers' reaper), `seaweedfs`, `toolbox`, `toxiproxy`, `valkey` and
`vault`. SeaweedFS is the catalog's S3 server: nixpkgs marks MinIO insecure,
abandoned upstream with unfixed vulnerabilities. Compose
files name these images explicitly; the engine never silently substitutes an
image name.

## Milestone — done

The first milestone shipped: two independent Compose stacks (`postgres` +
`redis` + a tiny app each) run concurrently on a macOS CI runner, serve
queries on different assigned ports, cannot reach each other's ports or
paths, and `down` on one leaves the other working and frees its ports.
Later pull requests widened the Docker API, Compose and Testcontainers
surface; each landed with its tests on the macOS runner.

## Roadmap — Kubernetes in Docker (KinD)

`kind create cluster` against fwsd: the kind CLI starts each cluster node
as a privileged container from a node image, and the node runs a
Kubernetes control plane and kubelet whose pods are containers. The
official `kindest/node` image is Linux (systemd, containerd, kubelet), so
it needs a VM; the roadmap builds the same contract from macOS parts, in
dependency order:

1. **Nested engines and `fws/dind`** (done): a privileged
   container gets an engine of its own, which a node's pods run on, each
   in a sandbox the outer engine applies.
2. **Podman's API** (done): kind's `podman` provider, and
   `podman-compose`, drive fwsd as well as its `docker` provider does.
3. **Kubernetes control plane for darwin** (done): `etcd` (in the
   catalog), `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`,
   `kubectl` and `kubeadm` 1.35.0, the release kind 0.31's `kindest/node`
   ships, built with Nix from upstream's sources by upstream's make
   (`images/kubernetes`), and images for the four upstream publishes, over
   go-runner. A control plane of them turns Deployments into Pods, which
   wait for a node.
4. **A kubelet for macOS** (done): upstream's kubelet
   with the darwin parts its tree lacks (`images/kubernetes/kubelet-darwin.patch`):
   machine information and node CPU, memory and filesystem use from
   macOS's kernel in cAdvisor's shapes, a container manager for a node
   without cgroups, and the static pod manifests' watcher on kqueue. fwsd
   serves CRI on its sockets, nested engines' among them (`internal/cri`):
   pod sandboxes are `fws/pause` containers, their containers join the
   sandbox's network, and the engine writes CRI logs where the kubelet
   asks, and its stream server, on containerd's port 10010 in a node's
   container, serves exec, attach and port-forward. A kubelet in a
   privileged container joins the control plane as a node and runs
   Deployments' pods.
5. **Cluster networking and services**: pod addresses from the engine's
   IPAM per node network, a kube-proxy replacement that maps Service
   ClusterIPs and NodePorts onto the routers the engine already runs, and
   CoreDNS (from nixpkgs) answering cluster names through the container
   library's resolver.
6. **`fws/kind-node`**: the node image kind expects, at its paths
   (`/kind/version`, the kubeadm configuration kind writes, the manifests
   directory), with kubeadm's phases run against the components above in
   place of systemd units, so `kind create cluster --image fws/kind-node`
   brings up a single- and multi-node cluster.
7. **Tests**: kind's own create, load-image, export-logs and delete
   flows, `kubectl apply` of a Deployment and Service reached from the
   host, and Testcontainers' k3s module, on the macOS runner.

## Remaining work

Out of reach without root or a VM: Linux images, ICMP, PID and user
namespaces, `# syntax=` frontends and multi-platform builds, and identity or
memory limits the kernel enforces. Swarm is out of scope.

Also out of reach: a server seeing its stack's clients connect from their
member addresses rather than `127.0.0.1`. The kernel never sees member
addresses, so a server could learn a client's only from a record of the
client's source port, made before it connects. That takes binding an
ephemeral loopback port, which Seatbelt would then have to allow every
container, beyond the ports it is granted; a record made after the
connection would race the server's `accept`.

Open bug: a container can leave processes behind when it ends. The shim
ends the main process's group and every process whose environment carries
the container's tag. PostgreSQL's `pg_ctl` starts the server in a session
of its own, and the server writes its process titles over its arguments
and environment, so the server and its children carry neither the group
nor a readable tag. On CI, three children of the temporary server that
PostgreSQL's image runs to initialise the database outlived their
container, with launchd as their parent. The fix is a mark the kernel
keeps for the process's life, such as its Seatbelt sandbox, which every
descendant inherits and none can drop.

ClickHouse's temporary init server took the SIGTERM its image's
entrypoint sends once the init scripts ran, and its shutdown waited on its
TCP server's acceptor thread. The library drops a connection on one of its
listeners that writes nothing, its own probe of whether a listener lives,
and its `accept` then waited for the next connection, where Poco's
acceptor polls, accepts and checks whether it is to stop: the real
server's probe of the port left the init server's acceptor in `accept` for
good. Failing such an `accept` with `ECONNABORTED` instead made Poco log an
error for each probe, and the real server, which listens on `::` and then
on `0.0.0.0` and so probes its own listener, ended in `std::terminate`
with exit code 134. Handing the probe over as a connection its loopback client closed, as
Linux hands over a connection its client abandoned, failed too: on macOS
`getpeername` and `setsockopt` fail with `EINVAL` on a Unix socket whose
peer is gone, which Poco reports as errors, and the server again ended
with exit code 134. The probe that reached ClickHouse was its own: it
listens on `::` and then on `0.0.0.0` at one port, which the library binds
to one socket file, so the second bind found the file taken and probed
it. The library now refuses that bind with `EADDRINUSE`, as Linux does,
without a probe, when a socket of the process holds the file; it probes
only a file no socket of the process holds.
ClickHouse still met connections whose client had already closed, as a
check of whether its port listens leaves: Darwin fails `getpeername` and
`setsockopt` on a socket shut down both ways, where Linux names the
closed connection's client and sets its options. The library names the
client the router recorded, and sets `SO_NP_EXTENSIONS`'s
`SONPX_SETOPTSHUT` on every listener, which its connections inherit and
which keeps their options settable after a shutdown: the connections a
published port's TCP listener takes, as well as the library's own. The
crash report of the server that ended with 134 showed the connection
thread's exception reach the handler that ends the process, which
ClickHouse replaces once it has started listening.

A port a nested engine published inside a privileged container answered
`400 Bad Request`: the shim's proxy from the container's port directory to
the inner container passed on the client address the container's router
writes before each connection's stream, which only the library's
listeners read. The proxy now reads it.

The test of it checked that `wget -S -O-` printed the body last, where
wget prints its progress after the body; it now looks for the status line
and the body.

The Kubernetes test's dump of each component's log read the CLI's standard
output alone, where `docker logs` writes a container's standard error to
its own: etcd's and the kubelet's logs, all on standard error, came out
empty. It now reads both, with each container's state. They showed why
the node never registered: the kubelet's shell read the unquoted
`--eviction-hard=memory.available<10Mi` as a redirection from a file
named `10Mi`, and the container restarted with exit code 2 until the
test's limit. The test quotes the argument. The kubelet then ended at
start with exit code 1: Kubernetes' `hostutil` package, which the kubelet
asks to make its root directory's mount `rshared`, has no darwin
implementation, and its stub for other systems fails every call. The
kubelet patch adds one: macOS has one mount namespace, so every mount
reaches every process as `rshared` propagation makes it on Linux, and no
SELinux.
The node then registered, and its pods never started: the kubelet sets up
each pod's service account token in a projected volume, on an `emptyDir`
in memory, which it mounts as a tmpfs, and Kubernetes' `mount-utils` had
no darwin implementation either. The patch adds one that reads the
kernel's mount table with `getfsstat`, finds the mount point of a path
with `statfs`, and refuses to mount, which macOS allows no unprivileged
process; a memory-backed `emptyDir` is a directory on disk on darwin, as
a container's tmpfs is under the engine.

MongoDB's tests failed with `No such container` as they started the
container they had created: create took 10 s, cloning the image's root,
which mongosh's Node.js modules make large, once into place and once more,
under the engine's lock, as the record of the initial root that `docker
diff` compares against. The previous subtest's Testcontainers reaper,
which removes its session's containers once its connection drops, removed
the new one in that time. Create now makes both clones before it takes
the lock, side by side, and then copies into the record only what it
wrote into the root under the lock: the hosts files and the mount points.

The engine wrote into a container's root through links the image holds
as the host resolves them: a mount point, the hosts files, the home
directory, a build's `COPY` into a directory and a fresh volume's copy-up
all followed an absolute link to the host's path of that name, so an
image with `/data -> /Users/me` and a volume at `/data/x` replaced the
host's `/Users/me/x`. Each now resolves the path inside the root, as a
chroot would and as the container sees it.

Open bug: `docker-compose up --build`, which builds through `buildx bake`,
stalls for minutes after loading its bake definitions, while `docker build`
of the same Dockerfile takes seconds; `TestComposeWatch` takes 220 to 300
seconds against a five-minute limit. A command that outlives its limit now
prints every Go process's goroutines, to show what bake waits on.
The engine's log shows every BuildKit session failing its health check
with `only one connection allowed` and ending 10 s after it began, and
the test's `POST /grpc` taking 3 min 39 s. The sessions are no cause:
buildx opens two per build and closes the first at once, and BuildKit
notices a closed session at its second failed health check, 10 s on, as
it does under dockerd; a build of 15 s against the engine on Linux kept
its second session for 25 s and exported through it. A test of the
engine's HTTP server with BuildKit's session manager syncs a 64 MiB build
context through a session and checks its health after. The stall is
inside the solve.

Open bug: the end-to-end suite takes about 45 minutes, and a container
runs slower than the same program run natively. The engine's goal is
native speed, so an overhead of microseconds on a call that takes
nanoseconds natively is a bug. Two measurements find them:
`go test -bench . ./internal/pathmap` reports the nanoseconds a system call
takes through the path library beside the same call made natively, and
the suite runs the engine with `-debug` and ends with its time per
endpoint (requests, total, mean, longest). CRI's `ImageFsInfo` walked every
file of every image on each call; it now walks each image root once.

The first measurement on a macOS runner, natively and through the library:
`stat` 1.9 µs and 23.2 µs, `fstat` 0.84 µs and 18.4 µs, `stat` of a missing
file 1.4 µs and 5.9 µs, `readdir` 13.8 µs and 20.9 µs, `open` 7.7 µs and
9.6 µs, a process start 2.0 ms and 6.7 ms. Each `stat` read two extended
attributes, the owner and the mode, at about 9 µs apiece, whether the file
had them or not. The library now reads them only from a file marked with
`UF_NODUMP`, which `stat` returns with the rest; the image store and the
library mark each file they record either on, and the engine marks, once,
the files of a state directory an earlier engine wrote. Mapping a path
looked the file up twice, once for links on its way and once for whether
the container holds it; one lookup now settles a file the container holds
with no link on its way. A process start still costs 4.7 ms more than
natively.

A second measurement, with the flag: `fstat` 0.56 µs natively and
0.54 µs through the library, `stat` 1.09 µs and 2.58 µs, `stat` of a
missing file 1.01 µs and 4.65 µs, `readdir` 9.7 µs and 10.9 µs, a process
start 2.6 ms natively, 4.7 ms with an empty library inserted and 8.1 ms
with fwsroot: inserting any library costs the dynamic loader 2.1 ms, and
fwsroot 3.3 ms more. `stat`, `lstat`, `fstatat` and `open` without
`O_CREAT` now first make the call itself on the container's file with the
kernel refusing any link on the way, so a file the container holds costs
no lookup of the library's, and a missing one whose parent the container
holds costs one. The benchmark's `started` reports a process's time from
the kernel's start of it to `main`, to tell the loader's and the
constructor's share of a process start from the spawning call's.

A third measurement: `stat` 2.0 µs natively and 1.4 µs through the
library, `open` 8.5 µs and 9.1 µs, `fstat` and `readdir` as natively, a
missing file's `stat` 1.0 µs and 2.5 µs. A process reached `main` 2.9 ms
after the kernel started it natively, 7.3 ms with an empty library
inserted and 10.4 ms with fwsroot: the loader's 4.4 ms for an inserted
library and fwsroot's own 3.1 ms remain open.

The engine's time per endpoint over the suite, beside the requests that
wait on a container by design (`wait`, `attach`, `events`, BuildKit's
session): `docker rm` 2.38 s on average over 153 removals, a start 1.16 s
over 253, with a median of 1.165 s and a 90th percentile of 1.233 s, and a
create 368 ms over 278. A removal deleted the container's root, a clone of
its image's with as many files, under the engine's lock, which every
create, start and network change also takes; a create cloned the image's
root under it. A create now clones the root into a staging directory
before it takes the lock and moves it into place with a rename, and a
removal moves the container's directory to a trash directory under the
lock and deletes it after. With `-debug` the engine logs the time of each
step of a create and a start, and the shim the steps of its own start.
CRI's `ImageFsInfo` took 66 s on its first call and 4.1 s on average: it
walked every image's root once, and every container's root on each call.
The image store now measures a root once, as it unpacks it, and keeps the
measure in the image's record; CRI walks only a root an earlier store
unpacked, once. The walk of the containers' roots on each call remains:
each root is a clone of its image's, with as many files.

Every container start took about 1.15 s, nearly all of it in the shim's
step from starting the spawn helper to the helper's `exec` of the
container's command. `go test -bench . ./internal/runtime` measures that
start: 17 ms with the sandbox alone, 15 ms with the container library,
19 ms with the exec anchor and 21 ms with both, and 1.15 s through an
`fws-shim` built with the race detector, as the suite built it: a
race-built Go binary takes about half a second to start on macOS, and a
start runs two, the helper and the anchor's launcher. The suite now builds
fwsd alone with the detector. The suite's 46 image archives took about 7
of its 42 minutes to load into the race-instrumented engine, which
decompresses and unpacks each layer.

Open bug: a container's process starts in about 20 ms, where the same
`posix_spawn` natively takes 2 ms. The helper and the anchor's launcher
are Go processes, started one after the other, each with the runtime's
start-up; the library adds the loader's 4 ms for an inserted library.

Open bug: one start in an end-to-end run, of Testcontainers' reaper, took
3.8 s in its `profile` step, which takes about 80 µs in every other
start. The step reads the stack, IPC, shared-memory, PID-namespace and
network records, most under the engine's lock, so another request may
have held the lock that long; the debug log does not time the waits on
it.

Open bug: with a pids limit, every `fork` and `posix_spawn` lists every
process of the engine's user and checks each, to count the container's
against the limit, where a pids cgroup keeps a count. Docker sets no
limit by default, and no test of the suite sets one.

Left as they are, each with its reason in `LIMITATIONS.md`:

- tmpfs mounts are directories on disk. The one memory-backed filesystem
  macOS gives an unprivileged user, a RAM disk from `hdiutil`, takes a
  fixed size up front and mounts system-wide under `/Volumes`, where Finder
  and Spotlight see it: every container's tmpfs would appear on the host's
  desktop.
- `docker stats` counts payload bytes, without packet headers, and leaves
  out the disk I/O of a process that starts and ends between two samples:
  the kernel's per-socket and per-process counters hold no more.
- `--pids-limit` counts processes, not threads.
