# Contributing

fun-with-sandboxes is a Docker-API engine for macOS that runs containers as
the invoking user, confined by [Seatbelt](docs/SEATBELT.md) and rooted in
APFS clones. Read [PLAN](PLAN.md) for what it sets out to do,
[DESIGN](docs/DESIGN.md) for how it works, and
[LIMITATIONS](LIMITATIONS.md) for what it cannot do.

## Ground rules

A change that breaks one of [PLAN](PLAN.md#constraints)'s constraints is not
merged: no virtual machine, no root, Go that cross-compiles for
`darwin/arm64` with C only where a process start is at stake, and isolation
the kernel enforces and a test proves. Two more rules follow from them:

- **Refuse what cannot be honoured.** An option the engine cannot enforce
  returns a clear Docker API error. It is never accepted and silently
  ignored; [LIMITATIONS](LIMITATIONS.md) lists the few that still are, as
  bugs to fix.
- **Tests run everything they name.** No `t.Skip` for a missing tool:
  install the tool in CI instead.

## Set up

You need:

- **macOS** on Apple silicon, for everything beyond the portable unit
  tests;
- **Go**, at the version `go.mod` names;
- **the Xcode Command Line Tools** (`xcode-select --install`), whose clang
  builds the container library;
- **[Nix](https://nixos.org/download)**, to build the image catalog;
- **the Docker CLI, Docker Compose, buildx, Podman, `podman-compose` and
  `crictl`**, for the end-to-end suite:

  ```bash
  brew install docker docker-compose docker-buildx podman podman-compose cri-tools
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

[DESIGN §10](docs/DESIGN.md#10-go-implementation) lists every package.
Beyond them, `images/` holds the catalog's Nix specs and entry programs,
`scripts/` builds, publishes and vets them and shards the end-to-end suite,
`third_party/` holds the patched BuildKit, and `tests/e2e` the end-to-end
suite, a module of its own.

## Test

The checks CI runs, in the order to run them locally:

1. **Formatting:**

   ```bash
   test -z "$(gofmt -l .)"
   ```

2. **Vet**, for the macOS build the engine ships:

   ```bash
   GOOS=darwin GOARCH=arm64 scripts/vet.sh
   ```

3. **Unit tests.** On Linux these are the portable ones. On macOS they also
   build the container library and run probes under the real sandbox:

   ```bash
   go test -race -count=3 ./...
   ```

4. **End to end, on macOS.** Build the catalog's image archives, then run
   the suite against them:

   ```bash
   scripts/build-images.sh images-out
   ```

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 90m ./...
   ```

   One shard of it, as CI runs it:

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 75m -run "$(../../scripts/e2e-shard.sh 4 1)" ./...
   ```

   The suite starts an engine of its own, under a state directory of its
   own, so it leaves `~/.fws` alone. The engine loads each archive the
   first time a test asks for one of its images (`-image-archives`), so a
   single test or a shard loads only what it uses. It runs the engine with `-debug` and
   ends with the time the engine took to serve each endpoint.

CI runs these jobs on pull requests, pushes to `main` and by hand:

- `linux`: formatting, the cross-build, vet (the end-to-end module's too),
  the portable unit tests, that the shards hold every test once, and that
  `third_party/buildkit` matches its sync script;
- `macos`: the unit tests on macOS, what a container's processes pay
  for each system call the path library interposes, how long a
  container's main process takes to start, part by part, and how long
  Seatbelt takes to compile a profile;
- `servers`: the MySQL and MariaDB builds, cached by derivation;
- `images`: the catalog's archives and a local registry that takes them,
  cached under a hash of the files the build reads
  (`scripts/build-images.sh --inputs`), so a push that changes none of
  them skips the build. Otherwise it restores the newest earlier catalog
  and keeps each archive whose inputs are unchanged (`fws-mkimage
  -reuse`), so a change to one image rebuilds that one. It waits for
  `linux`, `macos` and `servers`, so a change the unit tests fail spends no
  image build or end-to-end run;
- `e2e`: the end-to-end suite, in four shards on runners of their own.
  `scripts/e2e-shard.sh` spreads the tests by their durations in
  `tests/e2e/durations.txt`, seconds per test from an earlier run; a test
  the file does not list counts as 30 seconds. Refresh the file from a
  run's `--- PASS` lines when a test's time changes much;
- `publish`: on `main`, once the suite passes, the archives to
  `ghcr.io/e6qu/fws-*`.

An image's entry program that imports the engine's packages is rebuilt,
with every image, whenever those packages change; keep what entries need
in small packages of their own, such as `internal/execpath` and
`internal/sandboxenv`.

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
  in the README's catalog and in the `images` job's registry check in
  `.github/workflows/ci.yml`, and drive it in `tests/e2e/catalog_test.go`
  through the client its users would use.
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
