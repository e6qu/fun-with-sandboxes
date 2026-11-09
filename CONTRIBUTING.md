# Contributing

fun-with-sandboxes is a Docker-API engine for macOS that runs containers as
the invoking user, confined by [Seatbelt](docs/SEATBELT.md) and rooted in
APFS clones. Read [PLAN](PLAN.md) for what it sets out to do,
[DESIGN](docs/DESIGN.md) for how it works, and
[LIMITATIONS](LIMITATIONS.md) for what it cannot do.

## Ground rules

These come from [PLAN](PLAN.md), and a change that breaks one is not
merged:

- **No virtual machines.** No Hypervisor.framework, no
  Virtualization.framework, no Containerization framework.
- **No root.** The engine, its install and every container run as the
  invoking user: no `sudo`, no root daemon, no kernel or system extension,
  no change to System Integrity Protection.
- **Go, cross-compiling.** The engine builds with `CGO_ENABLED=0` for
  `darwin/arm64` and `darwin/amd64` from any host. The container library is
  C, which the engine compiles on the user's Mac.
- **Isolation is proven, not claimed.** Each property is enforced by the
  kernel through Seatbelt, and a test tries to break it.
- **Refuse what cannot be honoured.** An option the engine cannot enforce
  returns a clear Docker API error. It is never accepted and silently
  ignored.
- **Tests run everything they name.** No `t.Skip` for a missing tool:
  install the tool in CI instead.

## Set up

You need:

- **macOS** on Apple silicon or Intel, for everything beyond the portable
  unit tests;
- **Go**, at the version `go.mod` names;
- **the Xcode Command Line Tools** (`xcode-select --install`), whose clang
  builds the container library;
- **[Nix](https://nixos.org/download)**, to build the image catalog;
- **the Docker CLI, Docker Compose and buildx**, for the end-to-end suite:

  ```bash
  brew install docker docker-compose docker-buildx
  ```

Run the engine from a checkout:

```bash
go build -o bin/ ./cmd/fwsd ./cmd/fws-shim
```

```bash
bin/fwsd
```

```bash
export DOCKER_HOST=unix://$HOME/.fws/docker.sock
```

## Layout

| Path | What it holds |
|---|---|
| `cmd/fwsd`, `cmd/fws-shim` | the engine, and the per-container supervisor |
| `cmd/fws-mkimage` | the tool that turns a Nix closure into an OCI image |
| `internal/api` | the Docker Engine API server |
| `internal/engine` | containers, images, volumes, networks, builds, execs |
| `internal/seatbelt` | the Seatbelt profile each container runs under |
| `internal/pathmap` | the container library (`src/fwsroot.c`) that presents a container's paths, users, hostname, ports and peers |
| `internal/runtime`, `internal/shim` | starting a container's processes and supervising them |
| `internal/alloc`, `internal/egress`, `internal/portproxy` | port blocks, the internet proxy, published ports |
| `internal/buildkit`, `third_party/buildkit` | BuildKit's solver with a darwin worker |
| `images/` | the catalog's images and the entry program each runs |
| `scripts/` | building, publishing and vetting |
| `tests/e2e` | the end-to-end suite, driven through the Docker CLI, Compose and Testcontainers |

## Test

The checks CI runs, in the order to run them locally:

1. **Formatting:**

   ```bash
   test -z "$(gofmt -l .)"
   ```

2. **Vet**, for the macOS build the engine ships:

   ```bash
   GOOS=darwin scripts/vet.sh
   ```

3. **Unit tests.** On Linux these are the portable ones. On macOS they also
   build the container library and run probes under the real sandbox:

   ```bash
   go test -race ./...
   ```

4. **End to end, on macOS.** Build the catalog's image archives, then run
   the suite against them:

   ```bash
   scripts/build-images.sh images-out
   ```

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 90m ./...
   ```

   The suite starts an engine of its own, under a state directory of its
   own, so it leaves `~/.fws` alone. It runs the engine with `-debug` and
   ends with the time the engine took to serve each endpoint.

CI runs these jobs on every push:

- `linux`: formatting, cross-builds, vet and the portable unit tests;
- `macos`: the unit tests on macOS, what a container's processes pay
  for each system call the path library interposes, and how long a
  container's main process takes to start, part by part;
- `servers`: the MySQL and MariaDB builds, cached by derivation;
- `images`: the catalog's archives and a local registry that takes them,
  cached under a hash of the files the build reads
  (`scripts/build-images.sh --inputs`), so a push that changes none of
  them skips the build;
- `e2e`: the end-to-end suite.

When `e2e` fails, its last step prints the crash reports and every distinct
Seatbelt denial. [SEATBELT](docs/SEATBELT.md) shows how to read the same
locally.

A flaky test is a bug: find the cause rather than retrying it. A test waits
on the event it needs, such as a Docker event, a health status or a
connection, never on a fixed sleep.

## Change things

- **The Seatbelt profile.** Add the rule to `internal/seatbelt/profile.go`,
  a check of its text to `profile_test.go`, and a probe that tries to break
  it to `sandbox_darwin_test.go`.
- **The container library.** `internal/pathmap/src/fwsroot.c` runs inside
  every container process, so:
  - It builds with `-Wall -Werror`.
  - Its path and socket mapping takes no lock libSystem may hold, so it
    calls no `snprintf`, `strtoul` or other locale-aware function there.
  - It keeps no thread-local state.
  - Each behaviour has a probe under `internal/pathmap/testdata`, run by
    `pathmap_darwin_test.go`.
- **The Docker API.** Answer as dockerd does, with the same status codes
  and error text, and add an end-to-end test that drives the change through
  the Docker CLI or Compose.
- **An image.** Add it to `scripts/build-images.sh` and `images/`, list it
  in the README's catalog, load it in CI's `e2e` job, and drive it in
  `tests/e2e/catalog_test.go` through the client its users would use.
- **BuildKit.** Change the packages it copies with
  `scripts/sync-buildkit.sh`, and patches in `third_party/buildkit-patches`.
  Never edit `third_party/buildkit` by hand: CI checks it matches the
  script's output.

Update the [README](README.md), [DESIGN](docs/DESIGN.md) and
[LIMITATIONS](LIMITATIONS.md) in the same change as the code they describe.

## Write

- **Commit messages** state the behaviour the change gives, in a sentence:
  "Stack members reach one another by name", not "Add member support".
- **Comments** say what the code cannot: a constraint, a quirk of macOS or
  of Docker, a choice whose obvious alternative is wrong. Write them in the
  active voice, and leave out what the next line already says.
- **Docs** use the active voice too, and name things by their real names:
  Seatbelt, APFS, Docker Compose.

## Send a pull request

Keep a pull request to one change you can describe in its title, and fill
its description with what changed and how it is tested. Every CI job must
pass before review.
