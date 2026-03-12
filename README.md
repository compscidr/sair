# SAIR — Shared Android Instrumented Runner

SAIR lets CI pipelines safely share physical Android devices. It provides
device locking, ADB protocol translation, and per-runner isolation so multiple
jobs never collide on the same device.

## Architecture

```
┌──────────────┐   ┌──────────────┐
│ DeviceSource │   │ DeviceSource │   (machines with USB-attached phones)
│  + Phone A   │   │  + Phone B   │
└──────┬───────┘   └──────┬───────┘
       │  gRPC            │  gRPC
       └────────┬─────────┘
                ▼
         ┌──────────────┐
         │ Orchestrator │              (central coordinator)
         └──────┬───────┘
                │  gRPC
                ▼
         ┌──────────────┐
         │    Proxy     │              (ADB protocol translator)
         └──────┬───────┘
                │  HTTP
                ▼
         ┌──────────────┐
         │  CI / Tools  │              (sair-acquire / sair-release)
         └──────────────┘
```

**DeviceSource** runs on each machine that has Android devices connected via
USB. It discovers devices through `adb` and exposes them over gRPC. You can run
device sources on as many machines as you like — they all register with a single
orchestrator.

**Proxy** translates the standard ADB protocol into orchestrator gRPC calls. CI
runners talk to the proxy using stock `adb` — no custom tooling required on the
runner side. The proxy does not need to be on the same machine as the device
sources; it only needs network access to the orchestrator.

**Tools** (`sair-acquire` / `sair-release`) are thin bash wrappers around the
proxy HTTP API. They handle locking, waiting for availability, and exporting the
environment variables that `adb` needs.

## Install

The quickest way to install is with the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash
```

This detects your OS and architecture, downloads the latest release, and
installs all binaries to `/usr/local/bin`. You can customize the install:

```bash
# Install a specific version to a custom directory
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash -s -- \
  --version v0.1.0 \
  --dir ~/.local/bin
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
attached to that specific machine. They all connect to the same orchestrator,
giving CI access to your entire device pool from a single proxy.

### 2. Orchestrator

The orchestrator is the central coordinator that tracks all device sources and
manages locks. It is available as a hosted service or can be self-hosted.

The orchestrator needs to know where each device source is running. Refer to the
orchestrator documentation for configuration details.

### 3. Proxy

Run the proxy on a machine that is reachable by your CI runners. It connects to
the orchestrator over gRPC.

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
┌─ Lab Machine A ────────────────┐
│  Phone: Pixel 8 (ABC123)       │
│  Phone: Pixel 7a (DEF456)      │
│  adb -P 5038 start-server      │
│  ./sair-device-source           │  ──► Orchestrator ◄── Proxy ◄── CI
└────────────────────────────────┘                          │
                                                            │
┌─ Lab Machine B ────────────────┐                          │
│  Phone: Samsung S24 (GHI789)   │                          │
│  adb -P 5038 start-server      │                          │
│  ./sair-device-source           │  ──► Orchestrator ◄─────┘
└────────────────────────────────┘
```

From CI's perspective, all three devices appear as a single pool. A
`sair-acquire` call locks whichever device(s) are free, regardless of which
machine they're physically connected to.

## License

See [LICENSE](LICENSE) for details.
