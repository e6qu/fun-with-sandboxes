# Contributing

How to set up, test and change fws. [PLAN](PLAN.md) says what it sets out
to do, [DESIGN](docs/DESIGN.md) how it works, and
[LIMITATIONS](LIMITATIONS.md) what it cannot do.

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in [RFC 2119](https://www.rfc-editor.org/info/rfc2119).

## Ground rules

A change MUST NOT break one of [PLAN](PLAN.md#constraints)'s constraints:
no virtual machine, no root, isolation the kernel enforces and a test
proves, and Go built for `darwin/arm64` with C only inside a process
start. Further rules:

- **Refuse what cannot be honoured.** An option the engine cannot enforce
  MUST return a Docker API error; it MUST NOT be accepted and silently
  ignored. Only an option that asks for no more confinement than a
  container already has MAY be accepted and change nothing;
  [LIMITATIONS](LIMITATIONS.md) lists them.
- **Tests run everything they name.** A test MUST NOT call `t.Skip` for a
  missing tool: install the tool in CI instead.
- **Flaky tests are bugs.** A flaky test's cause MUST be found and fixed
  rather than the test retried. A test MUST wait on the event it needs,
  such as a Docker event, a health status or a connection, and MUST NOT
  wait on a fixed sleep.
- **Requirements use RFC 2119.** Documentation and code comments use the
  RFC 2119 key words, uppercase, for requirements and only for
  requirements. A document that uses them MUST carry the boilerplate
  paragraph that RFC 2119 gives, as this one does above; a code comment
  follows the convention without repeating it.

## Set up

You need:

- **macOS** on Apple silicon, for everything beyond the portable unit
  tests and the cross-build;
- **Go**, at the version `go.mod` names;
- **the Xcode Command Line Tools** (`xcode-select --install`), whose clang
  builds the container library and the spawn helper;
- **[Nix](https://nixos.org/download)**, to build the image catalog;
- **[ShellCheck](https://www.shellcheck.net)** (`brew install shellcheck`),
  for `scripts/lint.sh`;
- for the end-to-end suite, the Docker CLI, Docker Compose, buildx,
  Podman, `podman-compose`, `crictl` and the Dev Containers CLI, and the
  toolchains the Testcontainers tests drive: Python 3, Java with Maven,
  Node.js, Bun and Rust's `cargo`:

  ```bash
  brew install docker docker-compose docker-buildx podman podman-compose cri-tools
  ```

  ```bash
  npm install -g @devcontainers/cli
  ```

Run the engine from a checkout; `fwsd` looks for `fws-shim` beside it
([USAGE](docs/USAGE.md#run-the-engine) has its flags):

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
Beyond them, `images/` holds the catalog's specs, Nix expressions and entry
programs; `scripts/` builds, packs and publishes the catalog, vets, lints
and fuzzes the Go code, shards the end-to-end suite and syncs BuildKit;
`third_party/` holds the vendored BuildKit and its patches; and
`tests/e2e` the end-to-end suite, a module of its own.

## Test

The checks CI runs, in the order to run them locally. The first three run
on any machine; the unit tests' probes and the end-to-end suite need macOS.

1. **Formatting and the cross-build:**

   ```bash
   test -z "$(gofmt -l .)"
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
   ```

2. **Static checks** for the macOS build: vet, staticcheck
   (`honnef.co/go/tools/cmd/staticcheck@v0.8.2`; `staticcheck.conf` names
   the checks left out) and govulncheck
   (`golang.org/x/vuln/cmd/govulncheck@v1.9.0`), each on `tests/e2e` too,
   with `-tags e2e`; and `scripts/lint.sh`, whose header lists the further
   checks it runs at pinned versions (ineffassign, nilness, exhaustive,
   unparam, unconvert, deadcode, a subset of gosec, ShellCheck and
   actionlint). A `switch` over one of the module's enum types names every
   value or has a `default`; one partial by design MUST carry a comment
   saying why and `//exhaustive:ignore` alone on the line above it.

   ```bash
   GOOS=darwin GOARCH=arm64 scripts/vet.sh
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 staticcheck $(go list ./... | grep -v /third_party/)
   ```

   ```bash
   GOOS=darwin GOARCH=arm64 govulncheck ./...
   ```

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

   Most packages come from Nix's binary cache; a first build also compiles
   what no cache holds for darwin (the `prebuilt` groups below), which
   takes hours on one machine.

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 90m ./...
   ```

   One shard of four, as CI runs it:

   ```bash
   cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e -v -count=1 -timeout 75m -run "$(../../scripts/e2e-shard.sh 4 1)" ./...
   ```

   The suite starts an engine of its own, under its own state directory,
   so it leaves `~/.fws` alone. That engine loads an archive the first time
   a test asks for one of its images (`-image-archives`), so a single test
   or a shard loads only what it uses. It runs with `-debug`, and the suite
   ends with the time the engine took to serve each endpoint.

### CI

`.github/workflows/ci.yml` runs these jobs on pull requests, pushes to
`main` and by hand:

- `linux`: formatting, the cross-build, the static checks, the portable
  unit tests, 30 s of each fuzz target, that the end-to-end shards hold
  every test once, and that `third_party/buildkit` matches its sync script.
- `macos`, on macOS 15 and 26: the unit tests; the probes again with the
  container library and spawn helper built under UndefinedBehaviorSanitizer
  (`FWS_CFLAGS`); the C sources under extra warnings and clang's static
  analyzer; two minutes of the path mapping fuzzer under AddressSanitizer
  and UndefinedBehaviorSanitizer (`TestPathMappingFuzz`, replayed with the
  printed `FWS_PATH_FUZZ_SEED`); the jetsam and Network.framework
  experiments; and benchmarks of each interposed system call, of a
  container's start and of compiling a profile.
- `prebuilt`: what Nix builds from source that no cache holds for darwin,
  one group per runner (`scripts/build-images.sh --prebuilt GROUP`):
  MySQL, MariaDB, the `images/catalog` packages at releases nixpkgs lacks,
  Kubernetes with etcd, and Consul. Each group is cached by its
  derivations and handed to `images` as an artifact; a run stopped partway
  keeps what it finished.
- `images`, after the three above: the catalog's archives, cached under a
  hash of the files the build reads (`scripts/build-images.sh --inputs`),
  so a push that changes none of them skips the build. Otherwise it
  restores the newest earlier catalog and rebuilds only the archives whose
  inputs changed (`fws-mkimage -reuse`), then checks that every tag
  `fws catalog -published` lists publishes to a local registry as a darwin
  image. `scripts/catalog-store.sh` packs the catalog with each layer once
  for the cache and the artifact the later jobs take.
- `e2e`: the end-to-end suite, in four shards on runners of their own.
  `scripts/e2e-shard.sh` spreads the tests by their durations in
  `tests/e2e/durations.txt`; a test the file does not list counts as 30
  seconds. Refresh the file from a run's `--- PASS` lines when a test's
  time changes much. A failed shard prints the run's crash reports and
  every distinct Seatbelt denial of its last 20 minutes;
  [SEATBELT](docs/SEATBELT.md) shows how to read the same locally.
- `publish`: on `main`, once the suite passes, the archives to
  `ghcr.io/e6qu/fws-*` (`scripts/publish-images.sh`).

## Change things

- **The Seatbelt profile.** A new rule goes in
  `internal/seatbelt/profile.go` and MUST come with a check of its text in
  `profile_test.go` and a probe that tries to break it in
  `sandbox_darwin_test.go`.
- **The container library.** `internal/pathmap/src/fwsroot.c` runs inside
  every container process, so:
  - It MUST build with `-Wall -Werror`; CI also compiles it, and the spawn
    helper, with `-Wextra -Wshadow -Wsign-compare`, and runs clang's
    static analyzer over the helper.
  - Its path and socket mapping MUST NOT take a lock libSystem might hold,
    so it MUST NOT call `snprintf`, `strtoul` or any other locale-aware
    function there.
  - It MUST NOT keep thread-local state.
  - Each behaviour MUST have a probe under `internal/pathmap/testdata`, run by
    `pathmap_darwin_test.go`.
  - `FWS_TRACE=1` in a container's environment traces its path mapping
    ([USAGE](docs/USAGE.md#tracing-paths)).
- **The Docker API.** An endpoint MUST answer as dockerd does, with the
  same status codes and error text, and a change MUST come with an
  end-to-end test that drives the change through
  the Docker CLI or Compose.
- **An image or a release.** See
  [IMAGE-AUTHORING](docs/IMAGE-AUTHORING.md#catalog-images).
- **BuildKit.** Change the packages it copies with
  `scripts/sync-buildkit.sh`, and patches in `third_party/buildkit-patches`.
  `third_party/buildkit` MUST NOT be edited by hand: CI checks it matches
  the script's output.
- **The docs.** A change MUST update, in the same pull request, every
  document that describes what it changes. Each fact lives in one
  document, which the others link to: the [README](README.md) lists
  which document holds what.

## Write

- **Commit messages** SHOULD state the behaviour the change gives, in a sentence:
  "Stack members reach one another by name", not "Add member support".
- **Comments** SHOULD say what the code cannot: a constraint, a quirk of
  macOS or of Docker, a choice whose obvious alternative is wrong. They
  SHOULD NOT repeat what the next line already says.
- **Docs** SHOULD use the active voice and name things by their real names:
  Seatbelt, APFS, Docker Compose.

## Send a pull request

A pull request MUST describe what changed and how it is tested. Every CI
job MUST pass before review. A pull request is squash-merged under a
one-line title.
