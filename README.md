# Openenterprise Bindicator

An over-engineered embedded bin collection indicator using TinyGo and Raspberry Pi Pico 2. The device monitors bin collection schedules via MQTT and indicates upcoming collections using LEDs.

![Three model bins under model street lights](_media/bindicator.png)

## Architecture

```
┌─────────────┐     MQTT      ┌───────────────┐    HTTPS    ┌─────────────────┐
│   Pico 2    │◄─────────────►│   Node-RED    │◄───────────►│ bins.felixyeung │
│ (Bindicator)│  1883 (TCP)   │ MQTT + HTTP   │   443       │      .com       │
└─────────────┘               └───────────────┘             └─────────────────┘
```

The lneto network stack doesn't support TLS, so MQTT over plain TCP to a local broker lets Node-RED handle the HTTPS API fetch.

## Hardware

- Raspberry Pi Pico 2 with CYW43439 WiFi
- 3 LEDs connected to GPIO pins:
  - **GP2**: Green bin LED
  - **GP3**: Black bin LED
  - **GP4**: Brown bin LED

## Features

### Core Functionality

- **Decoupled schedule refresh and LED processing** for responsive LED updates:
  - Wakes every 15 minutes (configurable) to process LED states
  - Fetches schedule via MQTT every 3 hours (configurable)
  - LEDs respond to 12-hour thresholds within 15 minutes instead of up to 3 hours
- **NTP time synchronization** for accurate timestamps:
  - Syncs time via NTP immediately after WiFi connection
  - Resyncs on each schedule refresh cycle (every 3 hours)
  - Uses UK NTP pool by default (configurable)
  - Ensures accurate telemetry timestamps from boot
- LED toggles ON 12 hours before collection (noon the day before)
- LED toggles OFF 12 hours into collection day (noon on collection day)
- Stores up to 15 scheduled jobs
- Maintains LED state on network errors (graceful degradation)

### Reliability

- Hardware watchdog with 8-second timeout
- Functional watchdog: resets after 3 consecutive MQTT failures or 12 hours without success
- Software reset fallback if hardware watchdog fails
- A/B partition system with automatic rollback (TBYB)

### Security

- Password-protected debug console (port 23)
- Password hidden during entry (telnet noecho)
- Constant-time password comparison (timing attack resistant)
- Progressive lockout: 5s after 3 failures, 30s after 5, 5min after 10
- OTA server disabled by default, auto-disables after transfer

### Connectivity

- CYW43439 WiFi with DHCP
- MQTT over TCP (plain, no TLS - use local broker)
- Random MQTT client ID to prevent conflicts with multiple units
- Telnet debug console with full IAC protocol support

### Telemetry

- OpenTelemetry-compatible logs, metrics, and traces
- OTLP/HTTP JSON format (port 4318)
- Automatic slog bridge for application logs
- Distributed tracing with trace context propagation
- See `docs/telemetry.md` for full documentation

## Watchdog & Recovery

The device has multiple layers of fault recovery:

| Layer               | Trigger                                   | Action                               |
| ------------------- | ----------------------------------------- | ------------------------------------ |
| Hardware watchdog   | 8 seconds without feed                    | System reset                         |
| Functional watchdog | 3 consecutive MQTT failures               | Stop feeding hardware watchdog       |
| Functional watchdog | 12 hours without successful refresh       | Stop feeding hardware watchdog       |
| Software fallback   | 15 seconds after fatal error              | Force reset via watchdog TRIGGER bit |
| OTA rollback (TBYB) | New firmware doesn't confirm within 16.7s | Revert to previous partition         |

On fatal errors (WiFi/DHCP failure, invalid config), the device:

1. Logs the error
2. Stops feeding the watchdog
3. Waits up to 15 seconds for hardware watchdog reset
4. Falls back to software reset if needed

## Configuration

### WiFi Credentials

Create these files in `credentials/`:

```
credentials/ssid.text     # Your WiFi SSID
credentials/password.text # Your WiFi password
```

### MQTT Broker

Create `config/broker.text` with your MQTT broker address:

```
192.168.1.100:1883
```

### MQTT Client ID (Optional)

Create `config/clientid.text` with an optional client ID prefix:

```
bindicator
```

The device appends a random hex suffix to prevent conflicts when running multiple units. If not specified, defaults to "bindicator".

### Console Password

Create `credentials/console_password.text` with a password for the debug console:

```
your-secure-password
```

The console uses progressive lockout after failed attempts (5s after 3 failures, 30s after 5, 5min after 10).

### Telemetry Collector (Optional)

Create `config/telemetry_collector.text` with your OTLP collector address:

```
192.168.1.100:4318
```

The device sends logs, metrics, and traces to this endpoint. If not configured, telemetry is disabled.

### Timing Configuration (Optional)

The device uses decoupled intervals for LED processing and schedule fetching:

**`config/wake_interval.text`** - How often to wake and process LEDs (default: 15m):

```
15m
```

**`config/schedule_refresh_interval.text`** - How often to fetch schedule from MQTT (default: 3h):

```
3h
```

This decoupling ensures LEDs respond to the 12-hour collection threshold within the wake interval (15 minutes by default), rather than waiting for the next schedule fetch (up to 3 hours). The schedule is cached between fetches, reducing network load while maintaining responsive LED updates.

### NTP Server (Optional)

Create `config/ntp_server.text` with your preferred NTP server (default: uk.pool.ntp.org):

```
uk.pool.ntp.org
```

The device syncs time via NTP immediately after WiFi connection and on each schedule refresh cycle. This ensures accurate timestamps for telemetry and LED timing from boot.

## MQTT Topics

| Topic                 | Direction       | Format                                          |
| --------------------- | --------------- | ----------------------------------------------- |
| `bindicator/request`  | Pico → Node-RED | `ping`                                          |
| `bindicator/response` | Node-RED → Pico | `TIMESTAMP,YYYY-MM-DD:TYPE,YYYY-MM-DD:TYPE,...` |

Example response: `1737207000,2026-01-17:BLACK,2026-01-31:GREEN,2026-02-14:BROWN`

### Time Synchronization

The device uses NTP as the primary time source, with MQTT timestamp as a fallback:

1. **NTP sync at boot** - Immediately after WiFi/DHCP, before telemetry initialization
2. **NTP resync** - On each schedule refresh cycle (every 3 hours by default)
3. **MQTT fallback** - If NTP fails, time is still synced from MQTT response timestamp

This is critical because:

- The Pico 2 has no RTC battery backup
- LED timing depends on accurate time (noon triggers)
- Telemetry requires accurate timestamps from boot
- Time resets to epoch (1970-01-01) on every reboot

The device uses `runtime.AdjustTimeOffset()` to set system time.

## Node-RED Flow

A sample flow is provided in `nodered/bindicator-flow.json`. Import it via Node-RED menu: Import → Clipboard → select file.

**Flow overview:**

```
MQTT In → HTTP Request → Transform to CSV → MQTT Out
(bindicator/request)   (bins API)      (function)     (bindicator/response)
```

**Configuration required:**

1. Update the MQTT broker connection to match your setup
2. Optionally change the premises ID in the HTTP request URL

**Transform function:**

```javascript
let data = JSON.parse(msg.payload);
let ts = Math.floor(Date.now() / 1000);
let jobs = data.data.jobs.map((j) => j.date + ":" + j.bin).join(",");
msg.payload = ts + "," + jobs;
return msg;
```

This converts the JSON API response to: `1737207000,2026-01-17:BLACK,2026-01-24:GREEN,...`

The Unix timestamp prefix is used to sync the device clock.

## Building

Requires TinyGo installed.

```bash
# Build UF2 firmware
make build

# Or directly:
tinygo build -o build.uf2 -target=pico2 -scheduler=tasks .
```

> **Important:** The `-scheduler=tasks` flag is required. The default `-scheduler=cores` is not supported and will cause runtime issues.

## Flashing

### Simple (no OTA support)

1. Hold BOOTSEL button on Pico 2
2. Connect USB while holding button
3. Copy `build.uf2` to the mounted drive
4. Device will reboot and start

### With Partition Table (for OTA support)

Requires picotool 2.2+ for partition support.

```bash
# First time setup: create partition table and flash
make flash-full

# Subsequent updates (device must be in BOOTSEL mode)
make flash

# View partition info
make partition-info
```

See `docs/ota.md` for full OTA documentation.

## CLI Tool

Build and use the CLI to interact with the device:

```bash
# Build CLI
make cli

# Single command
./bindicator-cli 172.18.1.156 version
./bindicator-cli 172.18.1.156 status
./bindicator-cli 172.18.1.156 refresh

# Interactive mode
./bindicator-cli 172.18.1.156
```

### CLI Authentication

The CLI needs the console password. Password sources (in priority order):

1. `-password` flag: `./bindicator-cli -host 172.18.1.156 -password secret -cmd status`
2. Environment variable: `BINDICATOR_PASSWORD=secret ./bindicator-cli 172.18.1.156 status`
3. `.env` file in current directory: `BINDICATOR_PASSWORD=secret`
4. Interactive prompt (if none of the above)

### OTA Commands

The CLI supports Over-The-Air firmware updates:

```bash
# Query device OTA status (current partition, enabled status)
./bindicator-cli 172.18.1.156 ota-info

# Enable OTA server manually (default 10 min timeout)
./bindicator-cli 172.18.1.156 ota-enable

# Push firmware update (auto-enables OTA first)
./bindicator-cli 172.18.1.156 ota-push build.uf2

# Inspect UF2 file locally (no device needed)
./bindicator-cli ota-file build.uf2
```

The OTA process:

1. CLI enables OTA server via console (auto-done by ota-push)
2. CLI extracts binary from UF2 and sends to device on port 4242
3. Device writes firmware to inactive partition (A→B or B→A)
4. Device verifies SHA256 hash
5. Device reboots to new partition
6. New firmware confirms partition within 16s (TBYB mechanism)

**Security:** OTA port 4242 is disabled by default and auto-disables after 10 minutes or after a successful update.

**Partition Boot Indicators:** On boot, LEDs briefly indicate which partition booted:

- Partition A: 2 slow blinks
- Partition B: 10 fast blinks

See `docs/ota.md` for full OTA documentation.

## Debug Console

Connect via telnet to port 23 (password required):

```bash
telnet <device-ip> 23
```

Or use the CLI tool which handles authentication automatically:

```bash
./bindicator-cli -host <device-ip> -cmd "status"
```

### Console Security

- Password set via `credentials/console_password.text`
- Password hidden during entry (telnet WILL/WONT ECHO negotiation)
- Progressive lockout after failed attempts:
  - 3 failures: 5 second lockout
  - 5 failures: 30 second lockout
  - 10+ failures: 5 minute lockout
- Constant-time password comparison prevents timing attacks

### Commands

| Command            | Description                                                     |
| ------------------ | --------------------------------------------------------------- |
| `help`             | Show available commands                                         |
| `version`          | Show version, git SHA, build date                               |
| `status`           | Show device status and job count                                |
| `net`              | Show IP address and uptime                                      |
| `wifi`             | Show WiFi quality (uptime, MQTT success rate, failures)         |
| `refresh`          | Trigger immediate schedule refresh                              |
| `time`             | Show current UTC time                                           |
| `jobs`             | List all scheduled collections                                  |
| `next`             | Show next upcoming collection                                   |
| `leds`             | Show current LED states                                         |
| `ota`              | Show OTA status (enabled, partitions, offsets)                  |
| `ota-enable [dur]` | Enable OTA server (e.g., `ota-enable 5m`, default 10m)          |
| `sleep [dur]`      | Set debug sleep duration (e.g., `sleep 1m`, `sleep 0` to reset) |
| `led-green`        | Toggle green LED                                                |
| `led-black`        | Toggle black LED                                                |
| `led-brown`        | Toggle brown LED                                                |
| `telemetry`        | Show telemetry status (queues, sent counts, errors)             |
| `telemetry-flush`  | Force immediate flush of telemetry queues                       |
| `ntp`              | Show NTP status (server, last sync, offset, sync count)         |
| `ntp-sync`         | Trigger immediate NTP time synchronization                      |
| `reboot`           | Reboot the device immediately                                   |

## Serial Monitor

Debug output is sent via USB serial at startup and during operation. Use TinyGo monitor or any serial terminal:

```bash
tinygo monitor
```

## Project Structure

```
├── main.go           # Entry point, WiFi/DHCP/main loop
├── bindicator.go     # LED control and schedule logic
├── mqtt.go           # MQTT client for Node-RED (includes time sync)
├── parse.go          # CSV response parser
├── console.go        # TCP debug console
├── cmd/
│   └── cli/          # CLI tool for interacting with device
│       └── main.go
├── config/
│   ├── config.go              # Config embedding
│   ├── broker.text            # MQTT broker address
│   ├── clientid.text          # MQTT client ID prefix
│   ├── telemetry_collector.text # OTLP collector address
│   ├── wake_interval.text     # LED processing interval (default: 15m)
│   ├── schedule_refresh_interval.text # MQTT fetch interval (default: 3h)
│   └── ntp_server.text        # NTP server hostname (default: uk.pool.ntp.org)
├── credentials/
│   ├── credentials.go
│   ├── ssid.text             # WiFi SSID
│   ├── password.text         # WiFi password
│   └── console_password.text # Debug console password
├── ota/
│   └── ota.go        # OTA update support (ROM function wrappers)
├── telemetry/
│   ├── telemetry.go  # OTLP telemetry (logs, metrics, traces)
│   ├── json.go       # Zero-allocation JSON serialization
│   └── slog.go       # slog.Handler bridge
├── partitions/
│   └── bindicator.json  # A/B partition table for OTA
├── docs/
│   ├── ota.md                # OTA system documentation
│   ├── debug-console.md      # Debug console implementation notes
│   └── telemetry.md          # Telemetry configuration and usage
├── version/
│   └── version.go    # Build info (injected via ldflags)
├── nodered/
│   └── bindicator-flow.json  # Node-RED flow for MQTT bridge
├── Makefile
└── README.md
```

## Dependencies

- [github.com/soypat/cyw43439](https://github.com/soypat/cyw43439) - CYW43439 WiFi driver
- [github.com/soypat/lneto](https://github.com/soypat/lneto) - Lightweight network stack
- [github.com/soypat/natiu-mqtt](https://github.com/soypat/natiu-mqtt) - MQTT client for embedded systems

> **Note:** Currently using lneto branch `tcp-tx-ack` ([PR #23](https://github.com/soypat/lneto/pull/23)) which fixes TCP ACK handling for persistent connections. Once merged, update to main with `go get -u github.com/soypat/lneto@latest`.

## Memory Usage

Static buffer allocation (~20KB total):

| Buffer             | Size       | Notes                      |
| ------------------ | ---------- | -------------------------- |
| TCP RX/TX (MQTT)   | 4060 bytes | Shared RX/TX               |
| MQTT decoder       | 512 bytes  | User buffer                |
| Console buffers    | 3072 bytes | RX + TX + work             |
| Job storage        | 80 bytes   | Max 15 jobs                |
| OTA chunk buffer   | 4096 bytes | Allocated during OTA       |
| OTA hash buffer    | 512 bytes  | Allocated during OTA       |
| Telemetry TCP      | 3072 bytes | RX + TX buffers            |
| Telemetry body     | 2048 bytes | JSON payload buffer        |
| Telemetry queues   | ~2KB       | Logs, metrics, spans       |

The firmware uses a zero-heap design with pre-allocated buffers for predictable memory usage on the Pico 2's 264KB RAM.

## Testing

```bash
# Run all tests
make test

# Run with verbose output
go test -v ./...
```

Tests cover:

- LED/schedule logic (`bindicator_test.go`)
- CSV response parsing (`parse_test.go`)
- UF2 extraction (`cmd/cli/ota_test.go`)
- Telemetry logs, metrics, spans (`telemetry/telemetry_test.go`)
- OTLP JSON serialization (`telemetry/json_test.go`)

## References

- [Bins API](https://bins.felixyeung.com/)
- [CYW43439 Examples](https://github.com/soypat/cyw43439/tree/main/examples)
- [TinyGo Tips](https://tinygo.org/docs/guides/tips-n-tricks/)
- [RP2350 Datasheet](https://datasheets.raspberrypi.com/rp2350/rp2350-datasheet.pdf)
