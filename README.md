# fun-with-sandboxes

A container engine for macOS that runs Docker Compose stacks side by side on
one Mac — without root and without a virtual machine. It serves the Docker
Engine API, so the stock `docker` CLI, `docker compose` and the Docker Go
client drive it unchanged.

Each container is a process tree on the host, confined by a generated
**Seatbelt** profile and rooted in an **APFS copy-on-write clone** of its
image. Each Compose project — a *stack* — gets its own block of host ports and
its own directory tree, so two stacks never clash and never see each other.

- [`PLAN.md`](PLAN.md) — constraints, isolation model, milestone
- [`docs/DESIGN.md`](docs/DESIGN.md) — the full mechanism
- [`LIMITATIONS.md`](LIMITATIONS.md) — what it cannot do, and why

## Run it

```bash
go install github.com/e6qu/fun-with-sandboxes/cmd/fwsd@latest
```

```bash
fwsd
```

```bash
export DOCKER_HOST=unix://$HOME/.fws/docker.sock
```

```bash
docker compose up -d --wait
```

`fwsd` refuses to run as root. State lives in `~/.fws` (`-root`), stacks get
host ports from 20000–29999 in blocks of 100 (`-port-base`, `-port-limit`,
`-block-size`).

## Images

Images are macOS-native (`darwin/arm64` or `darwin/amd64`). Linux images
cannot run without a VM. `scripts/build-images.sh` builds the catalog —
PostgreSQL 16, Redis 7 and a small `toolbox` app — from Nix packages into
archives for `docker load`, and `scripts/publish-images.sh` pushes them to a
registry the engine can pull from:

```bash
scripts/build-images.sh ./images-out
```

```bash
for t in ./images-out/*.tar; do docker load -i "$t"; done
```

`cmd/fws-mkimage` turns any Nix closure into an image: it rewrites the
closure's absolute `/nix/store` library paths and symlinks relative to each
file, so the image is self-contained wherever its root lands.

## The container contract

macOS gives an unprivileged process no private `/` and no private network
stack, so a container cannot pretend to own `/var/lib/postgresql/data` or port
5432. Instead the engine tells it where its things are:

| Variable | Meaning |
|---|---|
| `SANDBOX_ROOT` | the container's root directory on the host; image paths live under it |
| `SANDBOX_PORT_<n>` | the loopback host port to listen on for container port `<n>` |
| `SANDBOX_<SERVICE>_PORT_<n>` | the host port of stack peer `<service>`'s port `<n>` |
| `SANDBOX_<SERVICE>_HOST` | the host to reach that peer on (`127.0.0.1`) |
| `SANDBOX_STACK` | the stack (Compose project) name |

`PATH` entries in the image config are mapped into the root. Containers with
a network also get `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` pointing at
their stack's egress proxy; `network_mode: none` and `internal: true`
networks get none, as in Docker. Volumes and bind
mounts appear at their destination inside the root. An image's entrypoint
reads these variables and configures its service — see
`images/postgres/entry` and `internal/sandboxenv`.

## What a container can and cannot do

| | Allowed | Denied |
|---|---|---|
| Files | read and write its root and read-write volumes; read the macOS system tree | write anywhere else; read `/Users`, `/Volumes`, `/Library`, `/opt`, `/usr/local`, `/nix`, temp dirs, other stacks |
| Network | listen on its assigned loopback ports; connect to its stack's ports; reach the internet over HTTP(S) through its stack's egress proxy | listen on any other port or address; connect to anything else; reach the host through the proxy |
| Processes | exec binaries in its root and the system shell; signal its own processes | exec other host binaries; signal or inspect other sandboxes' processes |
| Mach services | user lookup, notifications, logging, certificate trust evaluation | keychain, pasteboard, Launch Services, TCC and everything else |

Requests the engine cannot honour without root — memory and CPU limits, user
switching, privileged mode, host networking, other network drivers — fail
with an error rather than being ignored.

## Testcontainers

Testcontainers drives the engine through `DOCKER_HOST` like any Docker
daemon. Its reaper, Ryuk, ships only as a Linux image, so turn it off:

```bash
export TESTCONTAINERS_RYUK_DISABLED=true
```

## Develop

```bash
go test ./...
```

On macOS the Seatbelt package also runs a probe under the real
`sandbox-exec`. The end-to-end suite needs the image archives, the Docker CLI
and Docker Compose:

```bash
cd tests/e2e && FWS_E2E_IMAGES=../../images-out go test -tags e2e ./...
```
