# SAIR — Shared Android Instrumented Runner

SAIR lets CI pipelines safely share physical Android devices. It provides
device locking, ADB protocol translation, and per-runner isolation so multiple
jobs never collide on the same device.

## Architecture

```
         ┌──────────────────────────┐
         │  DeviceSource            │
         │  + Phone                 │
         │  + real adb (port 5038)  │
         └────────────▲─────────────┘
                      │  gRPC
                      ▼
               ┌──────────────┐       ┌──────────────┐
               │    Proxy     │◄gRPC─▶│ Orchestrator │
               │  (port 5037) │       └──────────────┘
               └──────────────┘        (locks & sessions)
                  ▲       ▲
           ADB    │       │  HTTP
        (port 5037│       │(port 8550)
                  │       │
  ----------------+-------+---------------- CI runner --
                  ▼       │
         ┌────────────┐   │   ┌─────────────────┐
         │ adb client │   └───│ sair-acquire /  │
         │ (thinks it │       │ sair-release    │
         │  talks to  │       └─────────────────┘
         │  real adb) │
         └────────────┘
```

**DeviceSource** runs on each machine that has Android devices connected via
USB. It discovers devices through a real `adb` server (running on a non-standard
port like 5038) and registers with the proxy over gRPC. You can run device
sources on as many machines as you like.

**Proxy** is the central hub. Device sources register with it, and it discovers
devices and routes commands through them. It talks to the orchestrator over gRPC
for lock and session management. It listens on port 5037 — the standard ADB
port — so stock `adb` on CI runners thinks it's talking to a real ADB server.

**Orchestrator** manages device locks, sessions, and coordination. The proxy
talks to it over gRPC. It does not connect to device sources or use ADB
directly.

**Tools** (`sair-acquire` / `sair-release`) are thin bash wrappers that call
the proxy's HTTP API (port 8550) to acquire and release device locks.

**ADB client** — stock `adb` on the CI runner connects to the proxy on port
5037 (the standard ADB port). The proxy translates ADB protocol messages into
gRPC calls, so CI tools like `./gradlew connectedCheck` work without any
modification.

## Install

The quickest way to install is with the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash
```

This detects your OS and architecture, downloads the latest release, and
installs all binaries to `~/.local/bin`. You can customize the install:

```bash
# Install a specific version to a custom directory
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash -s -- \
  --version v0.1.0 \
  --dir /usr/local/bin
```

Or download a release archive directly from the
[releases page](https://github.com/compscidr/sair/releases).

## Prerequisites

- `adb` installed on every machine running a device source
- One or more Android devices connected via USB with USB debugging enabled
- A running orchestrator (see [Orchestrator](#orchestrator) below)

## Building from Source

Requires Go 1.24+.

```bash
go build ./cmd/sair-device-source
go build ./cmd/sair-proxy
```

Or with Docker:

```bash
# Device source image
docker build --target device-source -t sair-device-source .

# Proxy image
docker build --target proxy -t sair-proxy .
```

## Setup

### 1. Device Source

Run a device source on every machine that has phones attached. The device source
needs access to a local ADB server.

```bash
# Start a real ADB server on a non-default port (5038) to avoid
# conflicting with the proxy which will own port 5037.
adb -P 5038 start-server

# Start the device source
./sair-device-source
```

Environment variables:

| Variable | Default | Description |
|---|---|---|
| `DEVICE_SOURCE_PORT` | `8080` | gRPC listen port |
| `ADB_PORT` | `5038` | Port of the real ADB server |

Verify it's working:

```bash
grpcurl -plaintext localhost:8080 devicesource.DeviceSource/GetDevices
```

You can run device sources on multiple machines. Each one discovers the phones
attached to that specific machine and registers with the proxy, giving CI access
to your entire device pool.

### 2. Orchestrator

The orchestrator manages device locks, sessions, and coordination. It does not
connect to device sources or use ADB — the proxy handles all device
communication. The orchestrator is available as a hosted service or can be
self-hosted.

### 3. Proxy

Run the proxy on a machine that is reachable by your CI runners and device
sources. Device sources register with the proxy over gRPC, and the proxy
connects to the orchestrator over gRPC for lock/session management.

```bash
export ORCHESTRATOR_ADDR=your-orchestrator:9090
export SAIR_API_KEY=your-api-key
./sair-proxy
```

Environment variables:

| Variable | Default | Description |
|---|---|---|
| `ORCHESTRATOR_ADDR` | `localhost:9090` | Orchestrator gRPC address |
| `ORCHESTRATOR_TLS` | `false` | Use TLS for orchestrator connection |
| `SAIR_API_KEY` | `dev-key-123` | API key for authentication |
| `ADB_PROXY_PORT` | `5037` | ADB protocol listen port |
| `PROXY_HTTP_PORT` | `8550` | HTTP API listen port |
| `PROXY_HTTP_HOST` | `0.0.0.0` | HTTP API bind address |
| `SESSION_GRACE_PERIOD_MS` | `30000` | Grace period before releasing idle sessions |
| `HEARTBEAT_INTERVAL_SECONDS` | `60` | Lock heartbeat interval |

The proxy exposes two ports:

- **5037** (ADB protocol) — stock `adb` connects here, but sees no devices
  until a lock is acquired
- **8550** (HTTP API) — `sair-acquire` and `sair-release` call this to manage
  locks

### 4. Tools

Copy `tools/sair-acquire` and `tools/sair-release` into your CI project or add
this repo's `tools/` directory to `PATH`.

**Acquire** a device lock (blocks until devices are available):

```bash
eval $(sair-acquire)
```

After eval, these environment variables are set:

| Variable | Description |
|---|---|
| `SAIR_LOCK_ID` | Lock ID (passed to `sair-release`) |
| `SAIR_SERIALS` | Comma-separated list of acquired device serials |
| `ANDROID_ADB_SERVER_PORT` | Scoped ADB port — stock `adb` reads this automatically |
| `ANDROID_SERIAL` | First serial (only set when a single device is acquired) |
| `SAIR_PROXY_URL` | Proxy URL (for `sair-release`) |

**Release** the lock when done:

```bash
sair-release
```

Common options:

```bash
# Acquire specific device(s)
eval $(sair-acquire --serial DEVICE_A)
eval $(sair-acquire --serial DEVICE_A,DEVICE_B)

# Point to a remote proxy
eval $(sair-acquire --url http://proxy-host:8550 --api-key my-key)

# Release with explicit lock ID
sair-release --lock-id <lock-id>
```

## Deployment

SAIR components can run manually, as systemd services, or as Docker containers.
In all cases the real ADB server runs on the host (not in a container) on a
non-standard port.

### Manual

The simplest option — run everything directly on one machine:

```bash
# 1. Start a real ADB server on a non-default port
adb -P 5038 start-server

# 2. Start the device source
DEVICE_SOURCE_PORT=8080 ADB_PORT=5038 ./sair-device-source

# 3. Start the proxy (in another terminal)
ORCHESTRATOR_ADDR=sair.run SAIR_API_KEY=your-api-key ORCHESTRATOR_TLS=true ./sair-proxy
```

See [Setup](#setup) above for detailed configuration options.

### Systemd Services

Install binaries and systemd units:

```bash
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash -s -- --systemd
```

**Device source machine** — edit `/etc/sair/device-source.env`, then:

```bash
sudo systemctl enable --now sair-adb-server sair-device-source
```

This starts the real ADB server on port 5038 and the device source, both on boot.

**Proxy machine** — edit `/etc/sair/proxy.env` (set `ORCHESTRATOR_ADDR`,
`SAIR_API_KEY`, etc.), then:

```bash
sudo systemctl enable --now sair-proxy
```

Check status and logs:

```bash
sudo systemctl status sair-device-source
sudo journalctl -u sair-proxy -f
```

### Docker Containers

Pre-built images are published to GitHub Container Registry on each release:

```
ghcr.io/compscidr/sair-device-source:latest
ghcr.io/compscidr/sair-proxy:latest
```

**Device source machine** — the real ADB server must run on the host (not in a
container). Use the systemd unit to start it on boot, or start it manually:

```bash
# Option A: Install the ADB systemd service (starts on boot)
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash -s -- \
  --systemd-adb-only

# Option B: Start manually
adb -P 5038 start-server
```

The device source container needs to reach the host's ADB server:

```bash
# Run device source container
docker run -d --name sair-device-source \
  --network host \
  -e ADB_PORT=5038 \
  ghcr.io/compscidr/sair-device-source:latest
```

`--network host` is the simplest option — it lets the container reach the host's
ADB server on localhost:5038 and exposes the gRPC port directly.

**Proxy machine:**

```bash
docker run -d --name sair-proxy \
  -p 5037:5037 \
  -p 8550:8550 \
  -e ORCHESTRATOR_ADDR=your-orchestrator:9090 \
  -e SAIR_API_KEY=your-api-key \
  ghcr.io/compscidr/sair-proxy:latest
```

Pin to a specific version by replacing `latest` with a release tag (e.g.
`v0.1.0`).

## Example: GitHub Actions Workflow

```yaml
name: Android Tests

on: [push, pull_request]

jobs:
  connected-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21

      # Fetch the SAIR tools
      - uses: actions/checkout@v4
        with:
          repository: compscidr/sair
          path: sair
          sparse-checkout: tools

      - name: Acquire device
        env:
          SAIR_PROXY_URL: ${{ vars.SAIR_PROXY_URL }}
          SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
        run: |
          ACQUIRE_OUTPUT=$(sair/tools/sair-acquire)
          eval "$ACQUIRE_OUTPUT"
          # Re-export for subsequent steps
          echo "SAIR_LOCK_ID=$SAIR_LOCK_ID" >> "$GITHUB_ENV"
          echo "SAIR_SERIALS=$SAIR_SERIALS" >> "$GITHUB_ENV"
          echo "SAIR_PROXY_URL=$SAIR_PROXY_URL" >> "$GITHUB_ENV"
          echo "ANDROID_ADB_SERVER_PORT=$ANDROID_ADB_SERVER_PORT" >> "$GITHUB_ENV"

      - name: Run connected tests
        run: ./gradlew connectedCheck

      - name: Release device
        if: always()
        env:
          SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
        run: sair/tools/sair-release
```

Key points about the workflow:

- `sair-acquire` blocks until a device is available, so jobs queue naturally
  when all devices are busy.
- `ANDROID_ADB_SERVER_PORT` tells stock `adb` to connect to the proxy's scoped
  port, which only exposes the locked devices.
- `sair-release` is in an `if: always()` step so the lock is freed even when
  tests fail.
- Use `ACQUIRE_OUTPUT=$(sair-acquire)` instead of `eval $(sair-acquire)` to
  propagate exit codes correctly, then eval the output on success.

## Multi-Machine Example

A typical production setup with devices spread across multiple machines:

```
┌──────────────────────────┐   ┌──────────────────────────┐
│  Machine A               │   │  Machine B               │
│  DeviceSource            │   │  DeviceSource            │
│  + Phone A, Phone B      │   │  + Phone C               │
│  + real adb (port 5038)  │   │  + real adb (port 5038)  │
└────────────▲─────────────┘   └────────────▲─────────────┘
             │  gRPC                        │  gRPC
             └──────────┬───────────────────┘
                        ▼
                 ┌──────────────┐       ┌──────────────┐
                 │    Proxy     │◄gRPC─▶│ Orchestrator │
                 │  (port 5037) │       └──────────────┘
                 └──────────────┘        (locks & sessions)
                    ▲       ▲
             ADB    │       │  HTTP
          (port 5037│       │(port 8550)
                    │       │
  ------------------+-------+-------------- CI runner --
                    ▼       │
           ┌────────────┐   │   ┌─────────────────┐
           │ adb client │   └───│ sair-acquire /  │
           │ (thinks it │       │ sair-release    │
           │  talks to  │       └─────────────────┘
           │  real adb) │
           └────────────┘
```

From CI's perspective, all three devices appear as a single pool. A
`sair-acquire` call locks whichever device(s) are free, regardless of which
machine they're physically connected to.

## License

See [LICENSE](LICENSE) for details.
