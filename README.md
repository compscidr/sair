# SAIR — Shared Android Instrumented Runner

SAIR lets CI pipelines safely share physical Android devices. It provides
device locking, ADB protocol translation, and per-runner isolation so multiple
jobs never collide on the same device.

## Why

Without SAIR, every CI job that can see a phone uses it at the same time:

```
CI job 1 ──┐
           ├──▶ adb ──▶ Phone     ✗ both jobs install / uninstall the same APK
CI job 2 ──┘                      ✗ one job's reboot or settings change breaks
                                    the other's tests mid-run
                                  ✗ connectedCheck runs on every attached
                                    phone, including ones another job is using
```

With SAIR, a job must hold a lock before it sees a device, and waiting jobs
queue:

```
CI job 1 ──┐                 ┌──▶ Phone A (locked by job 1)
           ├──▶ Proxy ───────┤
CI job 2 ──┘      ▲          └──▶ Phone B (locked by job 2)
                  │ locks
CI job 3 ···  Orchestrator: job 3 waits in the queue until
                            a phone is released
```

## GitHub Action

This repo is also a GitHub Action that puts `sair-acquire`, `sair-release`,
`sair-wifi`, `sair-bluetooth`, `sair-locks` and `sair-label` on `PATH`. The tools need an orchestrator account: sign up at
[sair.run](https://sair.run) to get an API key, store it as the repository
secret `SAIR_API_KEY`, and set the repository variable `SAIR_PROXY_URL` to
your bench's proxy (see [Run](#run)). Then:

```yaml
steps:
  - uses: actions/checkout@v4
  - uses: compscidr/sair@v0.1.18
  - run: sair-acquire --count 1 > /dev/null
    env:
      SAIR_PROXY_URL: ${{ vars.SAIR_PROXY_URL }}
      SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
  - run: ./gradlew connectedCheck
  - if: always()
    run: sair-release --status ${{ job.status }}
    env:
      SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
```

The [full workflow example](#example-github-actions-workflow) explains each
step.

A runner with no proxy of its own (e.g. GitHub-hosted, using phones other
accounts share) can have the action start one, at the same release as the
action. Leave `SAIR_PROXY_URL` unset; the proxy listens on the default
`localhost:8550`, and its log is at `$RUNNER_TEMP/sair-proxy.log`:

```yaml
  - uses: compscidr/sair@v0.1.18
    with:
      proxy: true
    env:
      SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
```

Used at a branch or SHA rather than a release tag, the action warns and starts
the latest release instead.

## AI agents (MCP)

`sair-mcp` is an MCP server (stdio) for coding agents. It can run an app's
instrumented tests on a real phone in one call, or lease a phone and install
APKs, run shell commands, take screenshots and read logcat on it. It talks to
the orchestrator's agent API with an API key from [sair.run](https://sair.run).
The agent's machine needs no proxy or adb.

Install it with `go install github.com/compscidr/sair/cmd/sair-mcp@latest`,
take `sair-mcp` from a release tarball, or use `sair-mcp.mcpb` from a release
(an [MCP Bundle](https://github.com/modelcontextprotocol/mcpb) for clients
such as Claude Desktop). Then add it to your agent, e.g. Claude Code:

```bash
claude mcp add sair -e SAIR_API_KEY=sair_... -- sair-mcp
```

or any client's JSON config:

```json
{ "mcpServers": { "sair": { "command": "sair-mcp", "env": { "SAIR_API_KEY": "sair_..." } } } }
```

| Tool | Does |
|------|------|
| `run_tests` | Uploads `app_apk` and `test_apk` (paths on the agent's machine), waits for the run, returns per-test results, instrument output and a logcat tail |
| `list_devices` | Lists the phones the key may use: serial, model, `sdk` (API level), shared, in use |
| `lease_device` | Takes a phone (`serial`, `wait_s` optional) and returns the lease id |
| `install_apk` | Installs an APK on the leased phone, returns its package |
| `shell` | Runs a command, returns output and exit code |
| `screenshot` | Returns a PNG of the screen |
| `logcat` | Returns the most recent `lines` (default 500) |
| `release_device` | Ends the lease: uninstalls what it installed and frees the phone |

The server heartbeats the leases it takes, so an agent that thinks for a while
between calls keeps its phone. A lease still ends at the plan's maximum
(free 15 min, solo 1 h, team 2 h, enterprise 4 h). On a phone shared by another
user, shell commands are limited to a safe set (see the orchestrator README).
`SAIR_API_URL` points it at another orchestrator (default
`https://api.sair.run`).

It is listed in the [official MCP registry](https://registry.modelcontextprotocol.io)
as `io.github.compscidr/sair`. Every release publishes it: the release
workflow builds `sair-mcp.mcpb`, fills `server.json`'s version, download URL
and SHA-256 (the values in the repo are placeholders), and publishes it with
`mcp-publisher`, logged in through GitHub OIDC. Check it with
`curl "https://registry.modelcontextprotocol.io/v0/servers?search=io.github.compscidr/sair"`.

## Quick Start

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash
```

This detects your OS and architecture, downloads the latest release, and
installs all binaries to `~/.local/bin`.

### Prerequisites

- `adb` installed on the machine with Android devices
- One or more Android devices connected via USB with USB debugging enabled
  (don't sign them in to a personal account: see
  [Securing a bench](#securing-a-bench))

### Run

```bash
# 1. Start a real ADB server on a non-default port (5038), so it doesn't
#    conflict with the proxy which will own the standard port (5037).
adb -P 5038 start-server

# 2. Set your API key (shared by proxy and device-source)
export SAIR_API_KEY=your-api-key

# 3. Start the proxy and device source
sair-proxy &
sair-device-source
```

The proxy connects to the hosted orchestrator at `orchestrator.sair.run` by
default (with TLS). Override with `ORCHESTRATOR_ADDR` for local development.

### Use

Acquire a device lock (blocks until one is available), run your tests, then
release:

```bash
ACQUIRE_OUTPUT=$(sair-acquire)
eval "$ACQUIRE_OUTPUT"
./gradlew connectedCheck
sair-release
```

After eval, stock `adb` automatically talks to the proxy through the scoped
port and only sees the locked devices.

When the bench sets `WIFI_SSID` on the device source, Wi-Fi is managed for
you:

- **On acquire** each phone joins the bench Wi-Fi and the acquire returns only
  once the phone is *online* (below); it fails otherwise. `--no-wifi` hands it
  over *offline* instead, verified the same way rather than trusting the
  previous release. `--wifi-band 2.4|5|6` (also `sair-wifi on --band`) joins
  on that GHz band: the phone scans, the suggestion is pinned to the
  strongest BSSID of `WIFI_SSID` it sees in the band, and the join fails if
  it sees none. The phone's `cmd wifi status` shows the BSSID and frequency
  it joined.
- **On release**, and when a lock expires or is cancelled, each phone is reset:
  every suggestion added through `adb shell` and every saved network is
  forgotten and the radio is toggled, and the reset waits until the phone is
  offline with the radio back on. The next job never inherits the last one's
  network. The phone then rejoins the bench Wi-Fi, so it is online between
  leases and app updates can install (see the Play Store below); the next
  acquire puts it in the state that job asked for either way.
- **Also on release**, every third-party package the phone did not have when
  the device source first saw it is uninstalled (test APKs and `.test`
  packages included, with their app data and signed-in accounts), and the
  browser and mail apps are force-stopped; see `BASELINE_PACKAGES`,
  `RESET_FORCE_STOP` and `RESET_CLEAR_PACKAGES`. What was removed is written to
  the proxy log and the lock's log on the orchestrator, naming any Wi-Fi
  suggestion that went with a package. This runs before the Wi-Fi reset, so a
  suggestion a test app leaked for the bench network is gone before the reset
  waits for the phone to be offline.
- **A phone relayed to a borrower** resets differently: the first relay under a
  lock lists its third-party packages, and when the lock ends only what is not
  on that list is uninstalled, for the primary user only. An app on the phone
  before a lock's first relay stays; one installed during the lock goes, whoever
  installed it. `BASELINE_PACKAGES` has no effect on this reset, but a local or
  guest-key lock on the same phone still resets to the baseline, so list apps to
  keep in `BASELINE_PACKAGES`. What was removed goes to the proxy log and the
  bench events, not the lock's log, which is on the holder's proxy. Without that
  list (it could not be read, the device source is older than the proxy, or no
  relay got that far) nothing is uninstalled: the proxy logs a warning and
  reports a failed package reset. Leftovers can then survive until a local lock,
  because the next borrower's list includes them, as it does when a relay under
  a new lock arrives before the previous lock's reset (the proxy warns, naming
  what it suspects).
- **On acquire** the same cleanup runs first (reverse forwards, packages),
  before the Wi-Fi check. A job that never released, because it had no
  `if: always()` release step, its runner died, or the proxy was down when
  the lock expired, is cleaned up by the next job, whose lock log then shows
  `sair: uninstalled … (left by an earlier job)`. This is local locks only: a
  relayed lock's next job cleans up nothing.
- **No leaked UiAutomation**, on release and on acquire: a killed `am
  instrument` or a leftover uiautomator server can leave a UiAutomation
  registered, failing every later instrumentation with `UiAutomationService
  … already registered!`. A `uiautomator dump` probe detects it; sair then
  kills the shell user's `app_process`/`uiautomator` processes, which
  unregisters it, and reboots only if the probe still fails (never a phone
  with a secure lock). The lock log gets `sair: cleared a leaked UiAutomation
  registration (killed … | rebooted)`.
- **Awake and unlocked**, on acquire and again after the release reset: the
  screen is woken and the keyguard dismissed, then verified (`dumpsys power`
  says `Awake`, `dumpsys window policy` says the keyguard is not showing),
  with up to three attempts since a one-shot dismiss can race on some phones.
  A phone whose keyguard stays up fails the acquire with `screen locked`; that
  means a PIN, pattern or password is set, which sair cannot dismiss, so set
  the phone's screen lock to Swipe or None. The reason is also reported as
  the device's `warning` (proxy `/status`, device report) until a wake
  succeeds. A phone with `stay_on_while_plugged_in` off is logged as a
  warning, since it will sleep and re-lock between steps. Android 4.x phones
  (no `wm`) are verified through their own `mShowingLockscreen` field; a dump
  in a format sair does not know is logged once and treated as unlocked.
- **Rebooted on release** once its uptime is over `REBOOT_UPTIME_HOURS`
  (24 by default), while the phone is unacquired: the reset waits for the
  boot and the wake above before the next acquire of that phone can proceed,
  and the phone is not offered while it is down. The lock log gets
  `sair: rebooted after release (uptime …)` and the proxy's `/status` lists
  each phone's `last_reboot`. This runs before the Wi-Fi reset, so the
  offline wait is not spent twice. A phone with a PIN, pattern or password
  is never rebooted this way, since it would come back on "unlock to start"
  and need a human; the device source logs that with the fix. The wake and
  the reboot run whether or not the bench manages Wi-Fi.
- **Bluetooth cycled on release**, after the Wi-Fi reset: `svc bluetooth
  disable`, wait until `dumpsys bluetooth_manager` says the adapter is fully
  `OFF`, `svc bluetooth enable`, wait until it says `ON` (30 s each). A BLE
  stack an earlier job wedged (stuck advertisers, a scanner that never
  restarted) does not reach the next job as "No peers discovered". Disable
  alone often stops at `BLE_ON` (classic off, BLE still up for "Nearby device
  scanning" or an app that asked for BLE), so sair turns
  `ble_scan_always_enabled` off for the cycle and, if BLE is still held,
  force-stops the packages the dump lists as BLE apps (on One UI,
  `com.samsung.android.beaconmanager` and `com.samsung.android.mdx.kit`). A
  holder that survives that too (the Galaxy A06's Auto Hotspot scanner lives
  in `system_server`, listed as `android`) is cleared by airplane mode,
  scoped to Bluetooth through `airplane_mode_radios` so Wi-Fi and cell stay
  connected. The settings are restored before Bluetooth comes back on.
  Skipped after an uptime
  reboot, and done on acquire instead when no release reset ran since the
  phone's last job (or since the proxy started). The lock log gets
  `sair: Bluetooth cycled off and on (…)`. A radio that does not go fully off
  or come back on is in the lock log and is the device's `warning` until a
  cycle succeeds, but does not fail the acquire, since most jobs never use
  Bluetooth. Bluetooth is always left on: a radio that never goes fully off
  is still turned back on, and the line says so (`Bluetooth back on but never
  fully off: ... adapter is BLE_ON`). Phones without the adapter dump
  (Android 4.x) are skipped.
- **Mid-job**, `sair-bluetooth restart` runs the same cycle on the job's own
  phones and returns once the adapter is `ON`, for a suite that tests
  recovery from a Bluetooth stack restart (sair#156). A test that
  force-stops `com.android.bluetooth` itself instead races the cycle sair ran
  a minute earlier and gets stale adapter state. The lock log gets `sair:
  Bluetooth cycled off and on (…) (requested by the job)`; a failure is
  returned to the caller and the lock is kept. From the phone, a test calls
  it with the lock's device token like Wi-Fi; see
  [Device-side control](#device-side-control).
- **No Play Store updates during a lease**: on acquire the Play Store is
  disabled (`pm disable-user com.android.vending`) and on release it is
  enabled again, so updates install between leases, when the phone is back
  on the bench Wi-Fi (or mobile data). An update of Google
  Play services force-stops every app that uses it, the app under
  instrumentation included, which fails a job with `Process crashed`. The
  lock log gets `sair: Play Store disabled for the lease`. A phone that
  refuses (Xiaomi: `Cannot disable system packages`) says so in the lock log
  and keeps the lock. Phones without the Play Store and Android 4.x (whose
  `pm` crashes on it) are skipped. A job that needs the Play Store itself
  (billing, in-app updates, Play Integrity) cannot use it on these phones.
- **Rebooted when a radio is wedged**: each `svc wifi` / `svc bluetooth`
  toggle and radio state read is bounded (15 s). When the Wi-Fi reset or the
  Bluetooth cycle fails, the device source probes the radio's service
  (`dumpsys wifi` / `dumpsys bluetooth_manager`); only if that hangs too is
  the phone rebooted (same rules as the uptime reboot) and the reset run
  again. A phone that hangs on `svc` with a healthy radio is not rebooted.
  The lock log gets `sair: radio wedged (wifi|bluetooth), rebooted`, and a
  radio still bad after the reboot fails the reset as before.

A bench that was already running jobs has phones carrying the last job's
packages, so "first seen" would record those into the baseline. A test package
and its target, where `pm list instrumentation` names both and both are
installed, are never baselined, which covers CI leftovers without touching
apps that merely ship an instrumentation of their own; for anything else, or to
be certain, set
`BASELINE_PACKAGES` explicitly after one look at `pm list packages -3` on each
phone (set it empty to keep no third-party package at all). The device source
logs the baseline it recorded, by name, so an unwanted entry is visible in the
first log after the upgrade.
- **Mid-job**, `sair-wifi off` / `sair-wifi on` drop and restore the network
  without the job knowing the credentials, and return only once the phone is
  offline or online. Each phone is switched on its own, so two phones can ask
  for opposite states at the same time.
- **From the phone**, a test can toggle its own Wi-Fi between two assertions
  with the lock's device token; see [Device-side control](#device-side-control).

**Online** means all of: associated to `WIFI_SSID` itself (not a SIM or a
leftover network answering instead), that Wi-Fi is Android's validated default
network (the same verdict the app under test gets), and `WIFI_PING_HOST`
answers, which with the default hostname proves DNS too. A VPN on top of that
Wi-Fi is fine. **Offline** means not on `WIFI_SSID` and `WIFI_PING_HOST` does
not answer; another network without an uplink, such as a test's own Wi-Fi
Direct group, still counts as offline. Each wait is bounded
(`WIFI_CONNECT_TIMEOUT_SECONDS`, `WIFI_DISCONNECT_TIMEOUT_SECONDS`) and a
timeout says which part failed, e.g. `associated to "Cafe", not "bench"`,
`connected but not VALIDATED`, or `still associated to "bench"`. Mobile data
is not managed: a phone with a SIM is never offline by this definition.

The device source counts each phone's Wi-Fi on attempts since it started:
failures by reason (`not_associated`, `other_ssid`, `not_validated`,
`no_route`, `other`), the last failure, and the latest, median and slowest
time to online. The proxy's `/status` lists them per phone as `wifi`, with a
`hint` when the slowest join comes near `WIFI_CONNECT_TIMEOUT_SECONDS` (raise
it) or most failures never associated (move the phone or the AP). A phone that
fails one join in ten, after at least five, is also logged as a warning.

A suggestion for the bench network registered by a test app's own uid is not
sair's to remove, but the app is: the package reset on release uninstalls it,
and Android drops its suggestions with it. Only a package in the baseline (a
test app that was on the phone when the device source first saw it) can keep
re-joining the bench Wi-Fi after every forget; `--no-wifi` acquires on that
phone then fail with `still associated to "bench"` until it is uninstalled or
left out of `BASELINE_PACKAGES`.

The Wi-Fi reset does not touch suggestions a test app registered under its
own uid; those go when the package reset uninstalls the app. Devices relayed
from another proxy are left alone.

The passphrase never goes on the adb command line: adbd logs every shell
service it serves to the phone's logcat, where the next job could read it, so
the device source feeds its commands to `sh` over stdin. A job that runs
`cmd wifi add-suggestion` itself through the scoped port has the command
redacted in the device source log and in the lock log.

When an acquire or a release reset fails on a phone, the lock log gets the
proxy's and the device source's recent log lines about that phone and lock
(`sair: proxy log before the failure`, and the same for the device source),
so the failure can be read from the portal without ssh to the bench. Lines
about the bench's other phones and locks are left out. The proxy also sends
the orchestrator small events on its session (acquire outcome and wait time,
Wi-Fi join time or failure cause, packages removed, reboots, versions, a
device source going stale) for the portal's per-phone view.

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
directly. A hosted orchestrator is available at `sair.run`.

**Tools** (`sair-acquire` / `sair-release`) are thin bash wrappers that call
the proxy's HTTP API (port 8550) to acquire and release device locks.

**ADB client** — stock `adb` on the CI runner connects to the proxy on port
5037 (the standard ADB port). The proxy translates ADB protocol messages into
gRPC calls, so CI tools like `./gradlew connectedCheck` work without any
modification.

## Configuration

### Device Source

| Variable | Default | Description |
|---|---|---|
| `DEVICE_SOURCE_PORT` | `8552` | gRPC listen port |
| `ADB_PORT` | `5038` | Port of the real ADB server |
| `DEVICE_SERIALS` | — | Only these serials (comma-separated) are managed; the rest of the bench's phones are invisible to sair. Set but empty: none |
| `EXCLUDE_SERIALS` | — | Every attached phone except these is managed. Mutually exclusive with `DEVICE_SERIALS` |
| `WIFI_SSID` | — | Bench Wi-Fi joined on acquire and reset on release; unset = Wi-Fi not managed |
| `WIFI_PASSPHRASE` | — | WPA2 passphrase (unset = open network) |
| `WIFI_PING_HOST` | `dns.google` | Must answer a ping when a phone is online and not when offline; a hostname also checks DNS, set an IP to skip that |
| `WIFI_CONNECT_TIMEOUT_SECONDS` | `60` | How long Wi-Fi on / acquire waits for a phone to be online |
| `WIFI_DISCONNECT_TIMEOUT_SECONDS` | `20` | How long Wi-Fi off / release / `--no-wifi` acquire waits for a phone to be offline |
| `BASELINE_PACKAGES` | — | Third-party packages kept on release, comma-separated. Unset: each phone's packages are recorded when first seen, minus installed test-package/target pairs. Set but empty: keep none. Set it explicitly on a bench that was in use before this version. No effect on a relayed lock's reset, which keeps what the phone had before that lock's first relay |
| `RESET_FORCE_STOP` | `com.android.chrome,com.google.android.gm` | Apps force-stopped on release |
| `RESET_CLEAR_PACKAGES` | — | Apps whose data is cleared (`pm clear`) on release. Off by default: clearing Chrome on a signed-in phone signs it out |
| `REBOOT_UPTIME_HOURS` | `24` | Reboot a phone on release once it has been up this long, before the next job gets it. `0` never reboots |
| `SAIR_LOG_LEVEL` | `info` | `debug` adds per-chunk byte counts for every forward, for tracing a stalled transfer |

A phone left out by `DEVICE_SERIALS` / `EXCLUDE_SERIALS` is never reported,
locked or reset (Wi-Fi, reverse forwards, packages), so a couple of phones can
stay on a CI bench for local development. The filter decides what sair
manages, not who owns the USB port: the bench's `sair-adb-server` still claims
every phone, so talk to an excluded phone through it
(`ANDROID_ADB_SERVER_PORT=5038`, or whatever `ADB_PORT` is) rather than
starting a second adb server that fights it for the devices. The device source
logs which attached phones it filtered out, and warns when `DEVICE_SERIALS`
names one that is not attached.

Verify it's working:

```bash
grpcurl -plaintext localhost:8552 devicesource.DeviceSource/GetDevices
```

### Proxy

| Variable | Default | Description |
|---|---|---|
| `ORCHESTRATOR_ADDR` | `orchestrator.sair.run:9090` | Orchestrator gRPC address (lock management) |
| `ORCHESTRATOR_TLS` | `false` (auto-enabled for hosted) | Use TLS for orchestrator connection |
| `SAIR_API_KEY` | `dev-key-123` | API key for authentication |
| `SAIR_GUEST_API_KEYS` | empty | Comma-separated API keys of **other** accounts allowed to lock this proxy's devices. Every key on the proxy's own account, and every other key on a listed guest's account, is accepted without being listed. See "Guest tenants" below. |
| `ADB_PROXY_PORT` | `5037` | ADB protocol listen port |
| `PROXY_HTTP_PORT` | `8550` | HTTP API listen port |
| `PROXY_HTTP_HOST` | `0.0.0.0` | HTTP API bind address |
| `HEARTBEAT_INTERVAL_SECONDS` | `60` | Lock heartbeat interval |
| `LOCK_IDLE_TIMEOUT_MINUTES` | `20` | Release a lock whose scoped port has had no adb client for this long (a job that died holding it); `0` disables |
| `PROXY_ID` | hostname | Identifies this proxy within the account so several machines can share one API key. Locks are granted only on devices this proxy reported. |
| `SAIR_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

The proxy exposes two ports:

- **5037** (ADB protocol) — stock `adb` connects here, but sees no devices
  until a lock is acquired
- **8550** (HTTP API) — `sair-acquire` and `sair-release` call this to manage
  locks

The proxy keeps one long-lived stream open to the orchestrator carrying its version, device reports, lock heartbeats and the ADB log of running jobs (flushed within a second). Against an orchestrator that predates the stream it falls back to the periodic unary calls automatically.

A lock can include devices connected to another of your proxies (or shared with you by another account). The orchestrator relays those ADB connections between the two proxies, so `adb` on the runner sees them like local devices. Only outbound connections are used, so proxies behind NAT work; the orchestrator is in the data path for remote devices only.

#### Guest tenants

Every key on the proxy's own account is accepted as it is: a second key you
create, or one an invited team member creates, works on your benches without
a config change. The proxy asks the orchestrator which account an unlisted
key belongs to and caches the answer for 30 seconds. Against an orchestrator
that predates this check, only `SAIR_API_KEY` and the guest keys below work.

One bench can serve two accounts. A typical case: a machine whose phones are
registered under a personal account also runs CI for a company account that
wants its own locks, queue, logs and billing. Set the company's API key as a
guest key on the proxy:

```bash
SAIR_API_KEY=owner-key                 # the account the phones belong to
SAIR_GUEST_API_KEYS=company-key,other  # accounts that may lock them here
```

A job holding a guest key uses the proxy like any other job (`sair-acquire`
with `SAIR_API_KEY=company-key` and the proxy's URL). The proxy forwards the
guest key on the lock request, so the orchestrator records the lock under
the guest account while the phones stay on the owner's proxy. ADB traffic
never leaves the machine.

**Trust model: listing a key is the owner's consent.** Guest keys live in the
proxy's own config, which only someone with access to that machine can edit.
Putting an account's key there says "this account may use these phones", so
nothing has to be marked shared in the portal. The devices stay out of
the public pool, and a guest can still lock them. The two mechanisms are
independent. Either way the holder can use any account signed in on the
phone, not just the test APK (see [Securing a bench](#securing-a-bench)):

| | Guest key on the proxy | Shared in the portal |
|---|---|---|
| Who may lock | Only the listed accounts | Every SAIR account |
| Where the lock runs | On the owner's proxy, locally | Through the orchestrator relay |
| `--count` / all | All phones on this proxy | Only the borrower's own phones |
| How to grant | Edit the proxy's environment | Toggle per device in the portal |

Both can be on at once. Owner, guest and public borrower all lock the same
physical phone in one lock domain, so no two can hold it together, and the
owner's jobs are served before anyone else's when a phone frees up.

A guest key is accepted on every proxy endpoint (`/acquire`, `/release`,
`/wifi`, `/label`, `/status`), the same as the owner's key. So is every other key on
a listed guest's account: listing one key grants that account, so the guest
can add or rotate keys without editing the proxy's environment (the listed
key must stay valid, since it is how the orchestrator finds the account).
Any other key is refused.
Guest keys need an orchestrator that understands them; an older one ignores
the field and records the lock under the owner.

### Tools

On GitHub Actions, `uses: compscidr/sair@v0.0.21` (or any later release tag)
puts the tools on `PATH`; see the workflow example below. Elsewhere, copy
`tools/sair-acquire`, `tools/sair-release`, `tools/sair-wifi`, `tools/sair-bluetooth`, `tools/sair-locks` and `tools/sair-label` into your CI project or add this
repo's `tools/` directory to `PATH`.

**Acquire** a device lock (blocks until devices are available):

```bash
eval $(sair-acquire)            # all devices
eval $(sair-acquire --count 1)  # any one free device
```

CI jobs that only need one phone should use `--count 1`, so the rest of the
pool stays available to other jobs.

After eval, these environment variables are set:

| Variable | Description |
|---|---|
| `SAIR_LOCK_ID` | Lock ID (passed to `sair-release`) |
| `SAIR_SERIALS` | Comma-separated list of acquired device serials |
| `ANDROID_ADB_SERVER_PORT` | Scoped ADB port — stock `adb` reads this automatically |
| `ANDROID_HOME` / `ANDROID_SDK_ROOT` | Shadow SDK under `$RUNNER_TEMP` (or `$TMPDIR`) whose `platform-tools/adb` calls the real adb with `-P <scoped port>`; only when an SDK was already set. Left in place so steps after `sair-release` keep working |
| `ANDROID_SERIAL` | First serial (only set when a single device is acquired) |
| `SAIR_PROXY_URL` | Proxy URL (for `sair-release`) |
| `SAIR_DEVICE_TOKEN` / `SAIR_DEVICE_API_PORT` | Lock-scoped token and the port on each phone that reaches the proxy's `/wifi` and `/bluetooth`; only when the proxy set them up. See [Device-side control](#device-side-control) |

**Release** the lock when done:

```bash
sair-release
```

Common options:

```bash
# Acquire specific device(s)
eval $(sair-acquire --serial DEVICE_A)
eval $(sair-acquire --serial DEVICE_A,DEVICE_B)

# Acquire any N free devices (mutually exclusive with --serial)
eval $(sair-acquire --count 2)

# Take every device from one proxy, for phones that must sit together (BLE,
# Wi-Fi Direct, adb reverse); waits until one proxy has enough free
eval $(sair-acquire --count 2 --same-proxy)

# Queue behind every default-priority job while devices are busy (higher
# goes first, ties are FIFO, default 0) — e.g. so bot PRs don't hold up humans
eval $(sair-acquire --priority -1)

# Phones verified offline (default: joined to the bench Wi-Fi and online)
eval $(sair-acquire --no-wifi)

# Join the bench Wi-Fi on one band of a dual/tri-band AP (2.4, 5 or 6 GHz)
eval $(sair-acquire --wifi-band 2.4)

# Tell matrix legs of one job apart (default: $SAIR_STAGE), and label the
# lease or one of its devices for the job views
eval $(sair-acquire --stage api-34 --label suite=smoke --label DEVICE_A:role=group-owner)

# Labels only known once the serials are granted; a key labeled again takes
# the new value (needs a proxy with /label)
sair-label DEVICE_A:role=group-owner DEVICE_B:role=client

# Drop and restore the network mid-job (all locked devices, or --serial)
sair-wifi off
sair-wifi on
sair-wifi on --band 5

# Restart the Bluetooth stack mid-job, sair's own off-then-on cycle; returns
# once the adapter is back on (all locked devices, or --serial)
sair-bluetooth restart

# Which release the tools (or a bench's binaries) came from
sair-acquire --version
sair-proxy --version   # also in the proxy's GET /status as "version"

# Point to a remote proxy
eval $(sair-acquire --url http://proxy-host:8550 --api-key my-key)

# Release with explicit lock ID
sair-release --lock-id <lock-id>

# Every lock your account holds (all proxies, all keys) and the ones that
# ended in the last hour, with how; asks the orchestrator, not the proxy
# (SAIR_ORCHESTRATOR_URL, default https://orchestrator.sair.run)
sair-locks
# Release every lock this key holds, e.g. after a crashed agent or killed job
sair-locks --release-mine
```

### Device-side control

Some tests know best when they need the network: this one needs Internet, the
next one must not have it, and the switch has to happen between two assertions
on the phone. The tenant API key must never go onto the phone (it unlocks
acquire/release for every job, and the next job can read what was left
behind), so a lock gets its own short-lived token instead.

On acquire the proxy mints a random **device token**, valid only for `POST
/wifi` and `POST /bluetooth`, only for serials in that lock, and only until the
lock ends. It also
runs `adb reverse tcp:<port> tcp:<port>` on each local phone, where `<port>` is
the proxy's HTTP API port, so `127.0.0.1:<port>` on the phone reaches the proxy
(this needs the proxy's HTTP API on the device source's host, which is the
normal bench layout). `sair-acquire` exports both as `SAIR_DEVICE_TOKEN` and
`SAIR_DEVICE_API_PORT`; pass them to the instrumentation along with the serial:

```bash
./gradlew connectedCheck \
  -Pandroid.testInstrumentationRunnerArguments.sairDeviceToken="$SAIR_DEVICE_TOKEN" \
  -Pandroid.testInstrumentationRunnerArguments.sairDeviceApiPort="$SAIR_DEVICE_API_PORT" \
  -Pandroid.testInstrumentationRunnerArguments.sairLockId="$SAIR_LOCK_ID" \
  -Pandroid.testInstrumentationRunnerArguments.sairSerial="$ANDROID_SERIAL"
```

On the phone a test then makes one plain HTTP call, no library needed. `on`
returns once the phone reaches the Internet, so the test can carry on as soon
as the call does; `off` drops the network:

```kotlin
fun sairWifi(on: Boolean) {
    val args = InstrumentationRegistry.getArguments()
    val url = "http://127.0.0.1:${args.getString("sairDeviceApiPort")}/wifi" +
        "?lock_id=${args.getString("sairLockId")}" +
        "&serial=${Uri.encode(args.getString("sairSerial"))}" +
        "&state=${if (on) "on" else "off"}"
    val conn = URL(url).openConnection() as HttpURLConnection
    conn.requestMethod = "POST"
    conn.setRequestProperty("x-device-token", args.getString("sairDeviceToken"))
    conn.readTimeout = 90_000 // "on" waits for Internet, up to the bench's timeout
    check(conn.responseCode == 200) { "sair wifi: HTTP ${conn.responseCode}" }
}
```

`POST /bluetooth?lock_id=…&serial=…&action=restart` with the same header
restarts the Bluetooth stack the way sair does on release and returns once the
adapter is back `ON` (sair#156); a recovery test calls it instead of
force-stopping `com.android.bluetooth` itself.

This is plain HTTP to `127.0.0.1`, so the app under test must permit cleartext
there: from targetSdk 28 the platform default refuses it with `Cleartext HTTP
traffic to 127.0.0.1 not permitted`. Allow it in the debug build's network
security config, or write the request line over a raw `Socket`, which the
policy does not cover.

With several phones in a lock, pass each its own serial (`SAIR_SERIALS` lists
them). The serial is required with a device token: the proxy cannot tell which
phone a reverse-forwarded connection came from. Devices relayed from another
proxy get no token access; their Wi-Fi is not managed at all. The forward is
removed with the lock, like every other `adb reverse` a job leaves behind.

## Securing a bench

A job holding a lock has full `adb shell` and instrumentation access to the
phone. Through UI automation that is enough to use any app the phone is signed
in to: mail, the browser's signed-in sessions, the Play Store. The proxy can't
filter this reliably, because a legitimate UiAutomator test does the same
things. **Whatever account is signed in on a bench phone is usable by anyone
who can lock it:** every key on your account (yours and your invited team's),
every key in `SAIR_GUEST_API_KEYS`, and every SAIR account if the phone is
shared in the portal.

- **Do the phones need an account at all?** adb installs, instrumentation,
  BLE, Wi-Fi Direct and Nearby Connections all work without one. The usual
  reasons to sign in are Play Store installs and Google Sign-In tests. If
  you don't need those, leave the phones signed out.
- **If they do, use a dedicated bench account, never a person's.** No payment
  method, Play purchases set to always require authentication, no real mail
  or contacts, and account recovery pointed at an alias. Assume anyone who
  can lock the phone can use the account.
- **A secondary Android user or work profile is not an isolation boundary.**
  The shell user can act across users, so don't rely on one to hide an
  account.
- **Guest keys and portal sharing hand out the whole phone,** not just the
  test APK. The holder can use whatever is signed in.

## Deployment

For production use, run SAIR components as systemd services or Docker
containers.

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

If the proxy or device source restarts in a loop with `fatal error: removed
GODEBUG ... set to old value`, the host OS is exporting a stale `GODEBUG` into
every service (UGOS sets `tlskyber=0`). The shipped units clear it with
`UnsetEnvironment=GODEBUG` (systemd ≥ 235); re-install them or add that line
in a drop-in (`sudo systemctl edit sair-proxy`).

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

      # Puts sair-acquire and sair-release on PATH, pinned to a release tag
      # (the action ships from v0.0.21 on). Bump the tag to upgrade; the
      # release notes list breaking changes.
      - uses: compscidr/sair@v0.0.21

      - name: Acquire device
        env:
          SAIR_PROXY_URL: ${{ vars.SAIR_PROXY_URL }}
          SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
        run: |
          # Also appends every variable to $GITHUB_ENV for the later steps
          sair-acquire --count 1 > /dev/null

      - name: Run connected tests
        run: ./gradlew connectedCheck

      - name: Release device
        if: always()
        env:
          SAIR_API_KEY: ${{ secrets.SAIR_API_KEY }}
        run: sair-release --status ${{ job.status }}
```

Key points about the workflow:

- `sair-acquire` blocks until a device is available, so jobs queue naturally
  when all devices are busy. `--priority N` orders that queue (higher first,
  FIFO within a value, default 0); pass e.g.
  `--priority ${{ github.actor == 'dependabot[bot]' && -1 || 0 }}` so bot PRs
  wait behind human ones.
- `--count 1` locks a single free device instead of the whole pool, leaving the
  other devices for concurrent jobs. It also sets `ANDROID_SERIAL`, so Gradle
  targets that one device.
- `ANDROID_ADB_SERVER_PORT` tells stock `adb` to connect to the proxy's scoped
  port, which only exposes the locked devices.
- `ANDROID_HOME` is repointed at a shadow SDK whose `adb` has the scoped port
  baked in. AGP 9.4+ runs instrumented tests in a Gradle worker daemon that
  starts with an empty environment, so `ANDROID_ADB_SERVER_PORT` never reaches
  the `adb` it shells out to; the shim makes that path work too. It needs
  `ANDROID_HOME` or `ANDROID_SDK_ROOT` to be set when `sair-acquire` runs, and
  warns on stderr when it is not.
- On GitHub Actions (`GITHUB_ENV` set) `sair-acquire` appends every variable
  it exports to `$GITHUB_ENV` itself, with raw values, so later steps see them
  without any parsing of the eval output.
- `sair-release` is in an `if: always()` step so the lock is freed even when
  tests fail. `--status ${{ job.status }}` records the outcome (success,
  failure, cancelled) on the job in the dashboards; without it a released job
  shows as unknown, and a lock that expires without a release shows as timeout.
- While a lock is held, the proxy records every ADB request made on the scoped
  port (the service string, when, how long the device session lasted, bytes
  returned) and ships it to the orchestrator on each heartbeat and on release,
  so the job pages show what actually ran on the device even for jobs that
  timed out. The bare port records nothing.
- On GitHub Actions `sair-acquire` also sends the repository, a link to the
  run, the job id, run attempt, commit, branch and workflow (from
  `GITHUB_REPOSITORY`, `GITHUB_RUN_ID`, `GITHUB_JOB`, `GITHUB_RUN_ATTEMPT`,
  `GITHUB_SHA`, `GITHUB_HEAD_REF` or `GITHUB_REF_NAME`, `GITHUB_WORKFLOW`),
  which the dashboards show per job. Elsewhere pass `--repo`, `--run-url`,
  `--job`, `--run-attempt`, `--sha`, `--branch` and `--workflow`.
  `GITHUB_JOB` is the same for every leg of a matrix job, so set `SAIR_STAGE`
  (e.g. `SAIR_STAGE: api-${{ matrix.api }}`) or pass `--stage` to tell the legs
  apart. A proxy older than these fields ignores them.
- Outside Actions, use `ACQUIRE_OUTPUT=$(sair-acquire)` instead of
  `eval $(sair-acquire)` so a failed acquire fails the step, then `eval` the
  output on success.

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

## License

See [LICENSE](LICENSE) for details.
