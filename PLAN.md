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
`etcd`, `gitea`, `influxdb`, `kafka`, `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, `kubectl`, `lambda`, `mailpit`, `mariadb`, `meilisearch`, `memcached`, `mongo`, `mysql`, `nats`,
`neo4j`, `nginx`, `node`, `opensearch`, `playwright`, `postgres`, `python`, `qdrant`, `rabbitmq`, `redis`,
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
4. **A kubelet for macOS**: upstream kubelet manages Linux cgroups and
   namespaces and does not run on macOS. The node runs a kubelet that
   speaks the API server's node and pod contract and runs pods through the
   node's nested engine: the pod's containers share a network as
   `--network container:` shares one, its volumes are the engine's
   volumes and bind mounts, and probes, logs, exec and port-forward pass
   to the engine. CRI, the kubelet's runtime interface, is served by fwsd
   for it.
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
