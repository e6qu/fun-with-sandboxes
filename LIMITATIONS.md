# Limitations

Every limit here follows from two constraints — no root and no virtual
machine — and from what macOS offers an unprivileged process under them.

## Images

- **macOS-native images only.** Nothing on macOS runs Linux ELF binaries
  without a VM. Stock Docker Hub images do not run.
- **Images follow the container contract.** A service reads its root and
  ports from `SANDBOX_*` variables (see the README). Software that hard-codes
  an absolute path outside its root, or a fixed port it cannot be told to
  change, needs an entrypoint that configures it.
- **No `#!` interpreters inside the image.** The kernel resolves a script's
  interpreter on the host; only the host's system shells are executable.
- **BuildKit builds darwin images on one worker.** `docker build`,
  `docker buildx build` (docker driver) and `docker compose build` run the
  real BuildKit solver with its cache. Not supported: multi-platform builds,
  `# syntax=` directives (gateway frontends ship as Linux images), and
  `RUN --mount=type=ssh`. Build steps run as the engine's user; a Dockerfile
  `USER` other than root fails.
- **The classic builder (`DOCKER_BUILDKIT=0`) has no cache**, no `ONBUILD`,
  no heredocs and no remote contexts.
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
- **Image health checks lose `StartInterval`**: the image config library the
  engine uses does not carry it. A container's own health check keeps it.

## Isolation

- **Not a security boundary against hostile code.** Containers run as the
  engine's user. Seatbelt confines them well enough to keep stacks apart and
  the host's data out of reach, but it is a development and test tool.
- **Process listings cross sandboxes.** `ps` inside or outside a container
  shows every process of the user; there is no unprivileged PID namespace.
  Signalling or inspecting another sandbox's process is denied.
- **System V IPC and POSIX shared memory names are machine-wide.** A
  container that guesses another's key or name could reach it. PostgreSQL
  derives its keys from its port, which differs per stack.
- **The macOS system tree is readable** (`/System`, `/usr/lib`, `/usr/share`,
  `/bin/sh` and friends): binaries cannot run without it, and it holds no
  user data.
- **The host's system shell is executable**, because `popen()` and
  `system()` hard-code `/bin/sh`.
- **A process that leaves its process group** (`setsid`) survives the
  container's stop; the engine kills the group, and macOS offers no
  unprivileged way to track a process across sessions.

## Networking

- **No container IP addresses.** Services talk over loopback on assigned host
  ports; `db:5432` in a client becomes `$SANDBOX_DB_HOST:$SANDBOX_DB_PORT_5432`.
- **One stack, one reachability domain.** A container can reach every port in
  its stack's block and nothing else. Networks shared across Compose projects
  are refused.
- **A host process that later binds a port inside a stack's block is
  reachable from that stack.** The allocator skips ports in use when it
  assigns them, but cannot stop other host software from binding them later.
- **Internet access is HTTP and HTTPS through a proxy.** Seatbelt cannot
  express "any host but this machine", so containers reach the internet only
  through their stack's egress proxy, which refuses destinations on the host.
  Software must honour `HTTP_PROXY`/`HTTPS_PROXY`; other protocols (raw TCP,
  UDP, DNS from the container) do not leave the machine. `fwsd -egress=false`
  turns the proxy off.
- **A published UDP port's concurrent clients are bounded by the stack's
  block.** Each client's flow sends to the container from a port of the
  stack's block, the only ports a container may send to, so a stack serves
  as many UDP clients at once as its block has ports to spare; a flow ends
  after 90 seconds without traffic, as with Docker's userland proxy. SCTP
  ports cannot be published.
- **Unix socket paths are capped at 104 bytes** on macOS. Keep the state root
  short (the default `~/.fws` is).

## Resources and identity

- **No memory, CPU, PID or block-I/O limits**; requests for them fail.
- **No user switching**: `user:` must be empty or the engine's own user.
- **`docker pause` stops the container's processes with SIGSTOP**, not a
  cgroup freezer; TCP connections to a paused container are still accepted
  by the kernel and wait for it to resume.
- **`docker update` changes restart policies only**; resource limits fail.
- **`docker top` and `docker stats` see the container's process groups**: its
  main process's and its running execs'. They read the host's `ps`, as dockerd
  does, so commands show their host paths. Stats report CPU time, resident
  memory and process count; there is no per-container network or block-I/O
  accounting, and the memory limit is the machine's memory.
- **`docker cp` copies the container's view**, mounts included, but refuses
  paths that resolve outside the container's root and mounts.
- **Testcontainers needs `TESTCONTAINERS_RYUK_DISABLED=true`.** Its reaper,
  Ryuk, ships only as a Linux image.
- **tmpfs mounts are directories** under the container's state, emptied at
  each start, not memory-backed.
