# ARP Entry Sharing Between MQTT and Telemetry

**Status**: Design needed
**Date**: 2026-01-31

## Problem

When MQTT broker and telemetry collector share the same IP (different ports), the current ARP cleanup causes conflicts:

```
Error: time=2026-01-31T18:34:47.995Z level=DEBUG msg=telemetry:logs-failed err="too many ongoing queries"
```

## Root Cause Analysis

### How lneto ARP Works

1. **No deduplication**: `StartQuery()` creates a new slot every time, even for the same IP
2. **FIFO discard**: `DiscardQuery(ip)` removes the **first** match (oldest entry)
3. **Manual cleanup**: Entries are never auto-freed; caller must discard explicitly
4. **Capacity limit**: `MaxQueries` slots available; "too many ongoing queries" when full

### The Conflict

| Scenario | What Happens | Result |
|----------|--------------|--------|
| **Discard after telemetry** | Removes first match = MQTT's entry | MQTT breaks |
| **Don't discard** | Telemetry leaks a slot per connection | Eventually hits MaxQueries |

### Timeline Example (Shared IP)

```
1. MQTT connects to 192.168.1.100:1883  → ARP slot #1 created
2. Telemetry connects to 192.168.1.100:4318 → ARP slot #2 created
3. Telemetry finishes, calls Discard(192.168.1.100)
   → Removes slot #1 (MQTT's entry!) ← BUG
4. MQTT's next packet fails ARP lookup
```

## Current (Broken) Fix

Added `protectedIP` to skip discarding for shared IPs:

```go
// telemetry/telemetry.go:657-661
if c.Addr() != protectedIP {
    s.DiscardResolveHardwareAddress6(c.Addr())
}
```

**Problem**: Still leaks slot #2, #3, #4... on each telemetry connection.

## Solution Options

### Option 1: Persistent Telemetry Connection (Recommended)

Make telemetry use a single persistent HTTP connection like MQTT does.

**Pros**:
- Only one ARP entry ever created
- Matches proven MQTT pattern
- No lneto changes needed

**Cons**:
- More complex connection management
- Need keepalive/reconnect logic

**Implementation**:
- Maintain persistent `*tcp.Conn` in telemetry module
- Reconnect on error instead of dial-per-request
- Never call `DiscardResolveHardwareAddress6`

### Option 2: Fix in lneto (Upstream PR)

Add deduplication to `DialTCP`:

```go
// Before StartQuery, check if entry exists
if existing := s.arp.QueryResult(ip[:]); existing != nil {
    // Reuse existing entry
    copy(mac, existing)
    return
}
err = s.arp.StartQuery(mac, ip[:])
```

**Pros**:
- Fixes the root cause
- Benefits all lneto users

**Cons**:
- Requires upstream acceptance
- Dependency on external project timeline

### Option 3: LIFO Discard Wrapper

Track our own entries and discard in reverse order:

```go
var telemetryARPSlots []netip.Addr

// After dial
telemetryARPSlots = append(telemetryARPSlots, ip)

// After close - discard newest first
if len(telemetryARPSlots) > 0 {
    last := telemetryARPSlots[len(telemetryARPSlots)-1]
    if last != protectedIP {
        s.DiscardResolveHardwareAddress6(last)
    }
    telemetryARPSlots = telemetryARPSlots[:len(telemetryARPSlots)-1]
}
```

**Pros**:
- Works with current lneto
- Discards telemetry's entries, not MQTT's

**Cons**:
- Doesn't solve the core duplication issue
- Complex state tracking

### Option 4: Accept Bounded Leakage

If `MaxQueries` is high enough relative to device uptime:

```
MaxQueries = 16
Telemetry interval = 30s
Slots leaked per hour = 120
Time to exhaust = 16/120 hours = 8 minutes  ← Too fast!
```

**Verdict**: Not viable unless MaxQueries is very high (100+).

## Recommendation

**Option 1 (Persistent Connection)** is the cleanest solution:

1. Aligns with MQTT's proven pattern
2. Reduces connection overhead (no repeated handshakes)
3. No external dependencies
4. Single ARP entry, never discarded

## Files to Modify

- `telemetry/telemetry.go`: Add persistent connection management
- `main.go`: Remove `SetProtectedIP` call (no longer needed)
- Revert current protectedIP changes

## Questions to Resolve

1. What's the current `MaxQueries` value in stack config?
2. Should telemetry HTTP use keepalive or just hold the connection open?
3. Error handling: retry on same connection or reconnect?

## References

- Original fix: `/workspace/docs/telemetry-connection-issues.md`
- lneto ARP handler: `github.com/soypat/lneto/arp/handler.go`
- lneto DialTCP: `github.com/soypat/lneto/x/xnet/stack-async.go`
