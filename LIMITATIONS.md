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
- **No `docker build`, `docker commit`, `docker push` or image export.**
  Images arrive through `docker load` or a registry pull.
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
- **UDP ports cannot be published**, and host port ranges are not supported.
- **Unix socket paths are capped at 104 bytes** on macOS. Keep the state root
  short (the default `~/.fws` is).

## Resources and identity

- **No memory, CPU, PID or block-I/O limits**; requests for them fail.
- **No user switching**: `user:` must be empty or the engine's own user.
- **No `docker pause` or `update`.**
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
