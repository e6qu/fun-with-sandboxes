# Contributing

fun-with-sandboxes is a Docker-API engine for macOS that runs containers as
the invoking user, confined by [Seatbelt](docs/SEATBELT.md) and rooted in
APFS clones. [PLAN](PLAN.md) says what it sets out to do,
[DESIGN](docs/DESIGN.md) how it works, and [LIMITATIONS](LIMITATIONS.md)
what it cannot do.

## Ground rules

A change that breaks one of [PLAN](PLAN.md#constraints)'s constraints is not
merged: no virtual machine, no root, isolation the kernel enforces and a
test proves, and Go built for `darwin/arm64` with C only inside a process
start. Two more rules follow:

- **Refuse what cannot be honoured.** An option the engine cannot enforce
  returns a Docker API error; it is never accepted and silently ignored.
  Only an option that asks for no more confinement than a container already
  has is accepted and changes nothing; [LIMITATIONS](LIMITATIONS.md) lists
  them.
- **Tests run everything they name.** No `t.Skip` for a missing tool:
  install the tool in CI instead.

## Set up

You need:

- **macOS** on Apple silicon, for everything beyond the portable unit
  tests;
- **Go**, at the version `go.mod` names;
- **the Xcode Command Line Tools** (`xcode-select --install`), whose clang
  builds the container library and the spawn helper;
- **[Nix](https://nixos.org/download)**, to build the image catalog;
- **[ShellCheck](https://www.shellcheck.net)** (`brew install shellcheck`),
  for `scripts/lint.sh`;
- for the end-to-end suite, **the Docker CLI, Docker Compose, buildx,
  Podman, `podman-compose`, `crictl`** and **the Dev Containers CLI**:

  ```bash
  brew install docker docker-compose docker-buildx podman podman-compose cri-tools
  ```

  ```bash
  npm install -g @devcontainers/cli
  ```

Run the engine from a checkout; `fwsd` looks for `fws-shim` beside it:

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

[DESIGN §10](docs/DESIGN.md#10-go-implementation) lists every package.
Beyond them, `images/` holds the catalog's Nix specs and entry programs,
`scripts/` builds and publishes the catalog, vets and fuzzes the Go code,
shards the end-to-end suite and syncs BuildKit, `third_party/` holds the
patched BuildKit, and `tests/e2e` the end-to-end suite, a module of its
own.

## Test

The checks CI runs, in the order to run them locally:

1. **Formatting and the cross-build:**

   ```bash
   test -z "$(gofmt -l .)"
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
   ```

2. **Vet, staticcheck and govulncheck**, for the macOS build the engine
   ships (`staticcheck.conf` names the checks left out). CI pins
   `honnef.co/go/tools/cmd/staticcheck@v0.8.2` and
   `golang.org/x/vuln/cmd/govulncheck@v1.9.0`, and runs all three on
   `tests/e2e` too, with `-tags e2e`:

   ```bash
   GOOS=darwin GOARCH=arm64 scripts/vet.sh
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 staticcheck $(go list ./... | grep -v /third_party/)
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 govulncheck ./...
   ```

   `scripts/lint.sh` runs the checks beyond those, at pinned versions:
   ineffassign, nilness, exhaustive (a `switch` over one of the module's
   enum types names every value or has a `default`; mark one partial by
   design with a comment saying why and `//exhaustive:ignore` alone on
   the line above it), ShellCheck over `scripts/` and actionlint over the
   workflows. None of them adds anything to what the engine runs:

   ```bash
   GOOS=darwin GOARCH=arm64 scripts/lint.sh
   ```

3. **Fuzzing**, of each `Fuzz` target for the time given; a failing input
   lands in its package's `testdata/fuzz` and runs with the tests after:

   ```bash
   scripts/fuzz.sh 30s
   ```

4. **Unit tests.** On Linux these are the portable ones. On macOS they also
   build the container library and run probes under the real sandbox:

   ```bash
   go test -race -count=3 ./...
   ```

5. **End to end, on macOS.** Build the catalog's image archives, then run
   the suite against them:

   ```bash
   scripts/build-images.sh images-out
   ```

   Most packages come from Nix's binary cache. A first build also compiles
   what no cache holds for darwin, CI's `prebuilt` groups below: hours on
   one machine.

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 90m ./...
   ```

   One shard of four, as CI runs it:

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 75m -run "$(../../scripts/e2e-shard.sh 4 1)" ./...
   ```

   The suite starts an engine of its own, under its own state directory,
   so it leaves `~/.fws` alone. The engine loads an archive the first time
   a test asks for one of its images (`-image-archives`), so a single test
   or a shard loads only what it uses. It runs with `-debug`, and the suite
   ends with the time the engine took to serve each endpoint.

CI (`.github/workflows/ci.yml`) runs these jobs on pull requests, pushes to
`main` and by hand:

- `linux`: formatting, the cross-build, vet, the portable unit tests,
  staticcheck, `scripts/lint.sh` and govulncheck (the end-to-end
  module's too), 30 s of each
  fuzz target, that the shards hold every test once, and that
  `third_party/buildkit` matches its sync script;
- `macos`, on macOS 15 and 26: the unit tests; the probes again with the
  container library and spawn helper built under UndefinedBehaviorSanitizer
  (`FWS_CFLAGS`); two minutes of the path mapping fuzzer under
  AddressSanitizer and UndefinedBehaviorSanitizer (`TestPathMappingFuzz`,
  replayed with the printed `FWS_PATH_FUZZ_SEED`); the jetsam and
  Network.framework experiments; and benchmarks of each interposed system
  call against the native one, of a container's start, part by part, and
  of compiling a profile;
- `prebuilt`: what Nix builds from source that no cache holds for darwin,
  in groups on runners of their own (`scripts/build-images.sh --prebuilt
  GROUP`): MySQL and MariaDB, with the compilation comment their vendors'
  builds carry (`images/servers.nix`); the catalog's packages at releases
  nixpkgs lacks (`images/catalog`); Kubernetes and etcd; and Consul. Each
  group is cached by its derivations and handed to `images` as an
  artifact. A group's run stopped partway, by a failure or a newer push,
  keeps what it finished, which the next run substitutes. A whole build
  drops the entries it supersedes, as `images` does, so the repository's
  10 GB of cache holds what the next run needs;
- `images`: the catalog's archives, cached under a hash of the files the
  build reads (`scripts/build-images.sh --inputs`), so a push that changes
  none of them skips the build. Otherwise it restores the newest earlier
  catalog and keeps each archive whose inputs are unchanged (`fws-mkimage
  -reuse`), so a change to one image rebuilds that one, and then checks
  every archive publishes to a local registry as a darwin image. It hands
  the archives to the run's `e2e` and `publish` jobs as an artifact,
  which another run's cache cannot evict past the repository's 10 GB, and
  drops the earlier catalog before it saves its own, as two would not fit
  beside the prebuilt groups. The whole of it substitutes from
  cache.nixos.org and the prebuilt groups, so no other store paths are
  cached, and the Go jobs keep no cache either. It waits for `linux`,
  `macos` and `prebuilt`, so a change the unit tests fail spends no image
  build or end-to-end run;
- `e2e`: the end-to-end suite, in four shards on runners of their own.
  `scripts/e2e-shard.sh` spreads the tests by their durations in
  `tests/e2e/durations.txt`, seconds per test from an earlier run; a test
  the file does not list counts as 30 seconds. Refresh the file from a
  run's `--- PASS` lines when a test's time changes much;
- `publish`: on `main`, once the suite passes, the archives to
  `ghcr.io/e6qu/fws-*` (`scripts/publish-images.sh`).

An image's entry program that imports the engine's packages is rebuilt,
with every image, whenever those packages change; keep what entries need
in small packages of their own, such as `internal/execpath` and
`internal/sandboxenv`.

When `e2e` fails, its last step prints the crash reports and every distinct
Seatbelt denial of the run's last 20 minutes.
[SEATBELT](docs/SEATBELT.md) shows how to read the same locally.

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
  - `FWS_TRACE=1` in a container's environment (`docker run -e`) has it
    write to standard error each call that asks about a file's place
    (`stat`, `getattrlist`, `statfs`, `realpath`), what the path mapped to
    and what came back.
- **The Docker API.** Answer as dockerd does, with the same status codes
  and error text, and add an end-to-end test that drives the change through
  the Docker CLI or Compose.
- **An image or a release.** See
  [IMAGES](docs/IMAGES.md#add-an-image-or-a-release).
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
  of Docker, a choice whose obvious alternative is wrong. Leave out what
  the next line already says.
- **Docs** use the active voice and name things by their real names:
  Seatbelt, APFS, Docker Compose.

## Send a pull request

Describe what changed and how it is tested. Every CI job must pass before
review; a pull request is squash-merged under a one-line title.
