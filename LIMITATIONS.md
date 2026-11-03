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
    package managers keep the library.
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
- **Process listings are presented, and pids are the host's.** There is no
  unprivileged PID namespace. Through the container library, a container's
  process listings (`sysctl`'s process table, libproc's pid lists) hold
  its own processes alone, its execs among them, and Seatbelt denies
  signalling or inspecting any other. But its processes keep their host
  pids, its main process is not pid 1, and a program that loads no
  library, as Apple's `ps` does not, lists every process of the user.
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
  - A member's port that it neither exposes nor publishes refuses every
    connection.
- **One stack, one reachability domain.** A container can reach every port in
  its stack's block and nothing else. Networks shared across Compose projects
  are refused.
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
  - Only ports the container exposes or publishes have host ports; a
    listener on any other port is refused, as is one on an address other
    than loopback's or the wildcard.
  - A dual-stack listener takes IPv4 clients alone: a client of `::1`
    falls back to `127.0.0.1`, as clients of `localhost` do.
  - Programs that load no library, as Apple's do not, see the host ports.
- **Unix socket paths are capped at 104 bytes** on macOS. Keep the state root
  short (the default `~/.fws` is).

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
  - `--pids-limit` kills processes started past the limit rather than
    failing their fork.
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
  leaves its disk I/O out. There is no per-container network accounting,
  and without a limit the memory limit shown is the machine's memory.
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
  ownership options are refused, as nothing would hold them.
- **Local volumes bind host directories or nothing else.** The local
  driver's `type=none,o=bind,device=<dir>` binds a host directory, which
  must exist; other filesystem types need root and are refused.
