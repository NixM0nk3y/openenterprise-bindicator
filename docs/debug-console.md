# Debug Console

The Bindicator includes a telnet-based debug console on port 23 for diagnostics and control.

## Connecting

```bash
# Using the CLI
./bindicator-cli <device-ip> <command>

# Or via telnet directly
telnet <device-ip> 23
```

## Available Commands

| Command | Description |
|---------|-------------|
| `help` | Show available commands |
| `version` | Show firmware version, git SHA, build date |
| `status` | Show system health, job count, failures |
| `net` | Show IP address, port, uptime |
| `wifi` | Show WiFi quality, MQTT success rate |
| `time` | Show current UTC time |
| `jobs` | List all scheduled bin collection jobs |
| `next` | Show next upcoming job |
| `leds` | Show current LED states |
| `led-green` | Toggle green LED |
| `led-black` | Toggle black LED |
| `led-brown` | Toggle brown LED |
| `refresh` | Trigger calendar refresh |
| `sleep <dur>` | Set sleep override (e.g., `sleep 30s`, `sleep 5m`) |
| `ota` | Show OTA update status |
| `ota-enable [dur]` | Enable OTA server (default 10 minutes) |

## TinyGo Timing Gotcha

### The Problem

TinyGo uses cooperative scheduling on single-core microcontrollers. When the console server was changed from `time.Sleep()` to `runtime.Gosched()` loops, TCP disconnect detection broke - subsequent CLI commands would timeout because the server never detected that the previous client had disconnected.

### Root Cause

Both `time.Sleep()` and `runtime.Gosched()` yield control to other goroutines, but they behave differently:

| Function | Yields | Waits | TCP Stack Gets Time |
|----------|--------|-------|---------------------|
| `time.Sleep(50ms)` | Yes | Yes (50ms) | Yes |
| `runtime.Gosched()` | Yes | No | No |

The lneto TCP stack needs **wall-clock time** to process packets, including FIN packets from client disconnects. Without this time:
- Client sends FIN when disconnecting
- Server's TCP stack never processes the FIN
- Connection stays in ESTABLISHED instead of transitioning to CLOSE_WAIT
- Server holds the connection open, blocking subsequent clients

### The Fix

After `conn.Flush()` operations, use `time.Sleep()` to give the TCP stack time to process packets:

```go
// Correct - gives TCP stack time to process
conn.Flush()
time.Sleep(50 * time.Millisecond)

// Wrong - yields but doesn't wait
conn.Flush()
for i := 0; i < 10; i++ {
    runtime.Gosched()
}
```

### Detecting Client Disconnects

The standard connection state checks don't catch all disconnect scenarios:

```go
// Incomplete - misses CLOSE_WAIT state
if conn.State().IsClosed() || conn.State().IsClosing() {
    return
}

// Complete - RxDataOpen() returns false in CLOSE_WAIT
if conn.State().IsClosed() || conn.State().IsClosing() || !conn.State().RxDataOpen() {
    return
}
```

The `RxDataOpen()` method returns false when the receive side of the connection is closed, which happens when the client sends a FIN packet (entering CLOSE_WAIT state).

### Summary

When working with TCP on TinyGo:
1. Use `time.Sleep()` after flush operations to allow packet processing
2. Check `!conn.State().RxDataOpen()` to detect client disconnects
3. Don't rely solely on `runtime.Gosched()` for timing-sensitive network operations
