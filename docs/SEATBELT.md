# Seatbelt

Seatbelt is the macOS sandbox. Apple's own name for it is the *sandbox*
(`Sandbox.kext`, `libsandbox`, `sandbox-exec`), and App Sandbox, Safari and
most system daemons run inside it. It is part of macOS, not a separate
project. GhostPack's *Seatbelt* on GitHub is an unrelated Windows tool.

fun-with-sandboxes isolates containers with Seatbelt because it is the one
confinement macOS gives an unprivileged process: no root, no kernel
extension of its own, no virtual machine. This page covers what Seatbelt
does, how to write and test a profile by hand, and how the engine uses one
for every container.

## How it works

A kernel extension checks each operation a sandboxed process makes against
the process's profile. The operations cover:

- reading and writing files;
- binding and connecting sockets;
- looking up and registering Mach services;
- POSIX and System V IPC;
- opening IOKit user clients;
- executing programs;
- signalling other processes.

The profile allows or denies each operation, and a denied one fails with
`EPERM` (`Operation not permitted`).

A process enters a sandbox once and cannot leave it:

- `sandbox-exec -f profile.sb command` runs a command under a profile.
- A program can sandbox itself with `sandbox_init` or
  `sandbox_init_with_parameters` from `libsandbox`.
- Every child the process forks or executes inherits the sandbox. A child
  can only narrow it further, by entering a sandbox of its own.
- No root is needed for any of it.

Apple marks `sandbox-exec` and the profile language deprecated and
undocumented, but both keep working, and macOS's own services depend on
them. The best references are Apple's profiles in
`/System/Library/Sandbox/Profiles/` and `/usr/share/sandbox/`.

## The profile language

Profiles are written in SBPL, a small Scheme dialect. A profile is a version
line followed by rules:

```scheme
(version 1)
(deny default)
(import "system.sb")
(allow file-read* (subpath "/usr") (literal "/etc/hosts"))
(allow file-write* (subpath "/tmp/work"))
(allow network-bind (local ip "localhost:8080"))
(allow network-outbound (remote ip "localhost:5432"))
```

- **`(deny default)`** denies every operation the profile does not allow;
  `(allow default)` allows every one it does not deny.
- **`(import "system.sb")`** pulls in Apple's baseline, the operations
  `dyld` and libSystem need to start any process at all.
- **Operations** are named, and a trailing `*` names a family:
  - `file-read*` covers `file-read-data` and `file-read-metadata`;
  - `file-write*` covers create, unlink, xattr and the rest.
  - Others in this page: `network-bind`, `network-outbound`, `mach-lookup`,
    `mach-register`, `ipc-posix-shm*`, `iokit-open-user-client`,
    `process-exec`, `signal`.
- **Filters** narrow a rule:
  - `(literal "/path")` matches one path, and `(subpath "/dir")` a directory
    and everything in it.
  - `(regex #"^/dev/ttys[0-9]+$")` matches a pattern.
  - `(local ip "localhost:8080")` and `(remote ip "localhost:5432")` match
    addresses; `localhost` matches `127.0.0.1` and `::1`.
  - `(global-name "com.apple.foo")` and `(global-name-prefix "fws.")` match
    Mach service names.
  - `(ipc-posix-name-prefix "fws1234.")` matches POSIX shared memory and
    semaphore names.
- **Parameters** keep paths out of the profile text: `(param "ROOT")` reads
  a value that `sandbox-exec -D ROOT=/some/dir` or
  `sandbox_init_with_parameters` supplies.

Two rules of precedence matter:

1. **Among rules for the same operation, the last one that matches wins.**
   So a profile denies broadly and then allows what it owns:

   ```scheme
   (deny file-write*)
   (allow file-write* (subpath "/tmp/work"))
   ```

2. **A rule with no filter outranks every filtered rule for its operation,
   wherever it stands.** `(allow file-read*)` would undo any later
   `(deny file-read* (subpath ...))`. To allow broadly and still deny
   within, give the broad rule a filter of its own: `(allow file-read*
   (subpath "/"))`.

Matching uses resolved paths. `/var` is a symbolic link to `/private/var`
and `/tmp` to `/private/tmp`, so a rule must name `/private/tmp/work` to
cover a file a program opens as `/tmp/work/file`.

## Tutorial

Each step runs on any Mac, as your own user, with nothing to install. Keep
a second terminal open to watch denials as they happen:

```bash
log stream --style compact --predicate 'sender == "Sandbox"'
```

### 1. Writes confined to one directory

Make a directory, resolved, and a profile that lets a process write there
and nowhere else:

```bash
mkdir -p /tmp/sb-tutorial/work && cd /tmp/sb-tutorial
```

```bash
cat > writes.sb <<'EOF'
(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath (param "WORK")) (literal "/dev/null"))
EOF
```

```bash
sandbox-exec -f writes.sb -D WORK=/private/tmp/sb-tutorial/work sh -c 'echo ok > /tmp/sb-tutorial/work/inside && echo wrote inside'
```

```bash
sandbox-exec -f writes.sb -D WORK=/private/tmp/sb-tutorial/work sh -c 'echo no > ~/outside'
```

The first command prints `wrote inside`. The second fails with `Operation
not permitted`, and the log stream shows a line like `sh(4242) deny(1)
file-write-create /Users/you/outside`.

### 2. Reads denied below an allowed tree

Allow reading everything, then deny one directory, and give the broad rule
a filter so the deny can take effect:

```bash
cat > reads.sb <<'EOF'
(version 1)
(allow default)
(allow file-read* (subpath "/"))
(deny file-read* (subpath (param "SECRET")))
EOF
```

```bash
mkdir -p secret && echo hidden > secret/file
```

```bash
sandbox-exec -f reads.sb -D SECRET=/private/tmp/sb-tutorial/secret cat secret/file
```

```bash
sandbox-exec -f reads.sb -D SECRET=/private/tmp/sb-tutorial/secret cat writes.sb
```

The first `cat` is denied, and the second reads `writes.sb`. Replace the
second line of `reads.sb` with a bare `(allow file-read*)` and both reads
succeed: the unfiltered rule outranks the deny below it.

### 3. Network confined to one port

Let a server listen on loopback port 8080 alone:

```bash
cat > net.sb <<'EOF'
(version 1)
(allow default)
(deny network-bind)
(allow network-bind (local ip "localhost:8080"))
EOF
```

```bash
sandbox-exec -f net.sb python3 -m http.server 8080 --bind 127.0.0.1
```

```bash
sandbox-exec -f net.sb python3 -m http.server 8081 --bind 127.0.0.1
```

The first server starts and serves `http://127.0.0.1:8080`. The second
fails with `PermissionError: [Errno 1] Operation not permitted`. Binding
`0.0.0.0:8080` fails too, because the rule names loopback alone.

### 4. Children inherit the sandbox

```bash
sandbox-exec -f writes.sb -D WORK=/private/tmp/sb-tutorial/work sh -c 'sh -c "echo nested > ~/outside"'
```

The inner shell is denied as its parent was. Nothing it executes, and no
environment it sets, takes it out of the sandbox.

### 5. Clean up

```bash
rm -rf /tmp/sb-tutorial
```

## How fun-with-sandboxes uses it

Every container process runs under a profile that `internal/seatbelt`
renders from the container's spec (`seatbelt.Spec`). Each section denies an
operation family and then allows what the container owns:

| Section | Allows |
|---|---|
| Baseline | `(deny default)` with Apple's `system.sb`, then forks, sysctl reads, ttys and sockets |
| File writes | the container's root, its writable volumes, and a few devices |
| File reads | everything under `/` except the engine's state directory, of which the container gets back its root, its volumes, the container library and its stack's network file |
| Execution | programs in the root and volumes, the system shells `popen()` and `system()` hard-code, and `fws-shim` once, to start the container's exec anchor |
| Network | Unix sockets in the root and volumes; binds on the loopback host ports behind the container's ports; connections to its stack's block of ports, the egress proxy included when the container has internet access |
| Mach services | lookups of a fixed list of Apple services (`MachServices`), and registering and looking up the container's own under its IPC prefix |
| IPC | POSIX shared memory and semaphores under the container's IPC prefix (`--ipc`) |
| Signals and process information | processes in the same sandbox only |
| IOKit and the rest | nothing: `(deny default)` keeps them denied |

The engine applies the profile without `sandbox-exec`.

- `fws-shim`, the container's supervisor, spawns each process through a
  small C helper.
- The engine compiles the profile once, with libsandbox's
  `sandbox_compile_string`, and keeps the bytecode beside the profile's
  text; the helper applies it with `sandbox_apply`, in microseconds, and
  then executes the container's program, so no Apple-signed binary sits in
  between.
- That matters because macOS strips `DYLD_INSERT_LIBRARIES` from programs
  Apple signs. With nothing in between, the container library (see
  [DESIGN](DESIGN.md)) reaches every container process.
- A process may signal only processes of its own sandbox, and a process
  that enters a profile by itself is a sandbox of its own, as step 4 of the
  tutorial shows from the other side. So the main process's helper starts
  an anchor inside the sandbox before it executes the program, and
  `docker exec` and health checks start through the anchor, in the same
  sandbox as the main process.

Seatbelt decides what a container may do; the library decides only what it
sees.

### Reading a container's profile

The engine writes each run's profile into the container's state directory,
`profile.sb` for the main process:

```bash
id=$(docker inspect -f '{{.Id}}' my-container | cut -c1-12)
```

```bash
cat ~/.fws/stacks/*/c/$id/profile.sb
```

Execs and health checks write `exec-<id>.sb` and `health-<id>.sb` beside
it.

To try a command under a container's exact confinement, run it with
`sandbox-exec -f` on that file. It runs without the container library,
though: it sees host paths and ports.

### Finding what a container was denied

Seatbelt logs every denial. While a container runs:

```bash
log stream --style compact --predicate 'sender == "Sandbox"'
```

Afterwards, each distinct denial once, by process:

```bash
log show --last 1h --style compact --predicate 'sender == "Sandbox"' | sed -n 's/.*Sandbox: \([^(]*\)([0-9]*) \(deny.*\)/\1 \2/p' | sort | uniq -c | sort -rn
```

CI prints the same summary whenever an end-to-end run fails.

### Changing the profile

Each rule has a test in `internal/seatbelt`:

- `profile_test.go` checks the rendered text: each rule is present, and in
  the order precedence needs.
- `sandbox_darwin_test.go` runs a probe under the real sandbox on macOS. It
  tries each operation a container should be denied, and each one it should
  be allowed.

A new rule ships with a probe that tries to break it. The end-to-end suite
in `tests/e2e` does the same with whole containers and stacks, for example
`TestTwoStacksRunSideBySideInIsolation`.

## Limits

Seatbelt confines a process. It does not virtualise one:

- **The host's view stays visible.** A container shares the host's process
  list, its loopback and its System V IPC keys.
- **Containers run as the engine's user.** Seatbelt stops a container from
  reading or writing what its profile denies. It is still no security
  boundary against code that sets out to attack the host:
  [LIMITATIONS](../LIMITATIONS.md) lists what that leaves open.
- **The interface is undocumented.** Apple can change Seatbelt's operations
  and filters between macOS releases. The rule tests in `internal/seatbelt`
  are what notice.
