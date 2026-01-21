# OTA (Over-The-Air) Firmware Updates

This document describes the OTA update system for the Bindicator, using the RP2350's native A/B partition system with Try-Before-You-Buy (TBYB) automatic rollback.

## Overview

The OTA system enables wireless firmware updates without physical access to the device:

1. Device runs from partition A or B
2. New firmware is written to the **inactive** partition
3. Device reboots to the new partition
4. New firmware must "confirm" within 16.7 seconds (TBYB)
5. If confirmation fails, bootrom automatically reverts to the previous partition

```
┌─────────────────────────────────────────────────────────┐
│                     Flash (4MB)                          │
├──────────────┬──────────────────────────────────────────┤
│ Partition    │ 0x000000-0x001FFF (8KB)                  │
│ Table        │ Created by picotool                       │
├──────────────┼──────────────────────────────────────────┤
│ Partition A  │ 0x002000-0x1F1FFF (1984KB)               │
│              │ Bootable firmware slot                    │
├──────────────┼──────────────────────────────────────────┤
│ Partition B  │ 0x1F2000-0x3E1FFF (1984KB)               │
│              │ Bootable firmware slot (linked to A)      │
├──────────────┼──────────────────────────────────────────┤
│ Reserved     │ 0x3E2000-0x3FFFFF (120KB)                │
└──────────────┴──────────────────────────────────────────┘
```

## Quick Start

### Pushing an OTA Update

```bash
# Build firmware
make build

# Check device OTA status
./bindicator-cli 172.18.1.136 ota-info

# Push firmware update
./bindicator-cli 172.18.1.136 ota-push build.uf2

# Verify update (after reboot)
./bindicator-cli 172.18.1.136 version
```

### CLI Commands

| Command | Description |
|---------|-------------|
| `ota-info` | Query device OTA status (partitions, offsets, enabled) |
| `ota-enable [dur]` | Enable OTA server (default: 10m timeout) |
| `ota-push <file.uf2>` | Push firmware update (auto-enables OTA first) |
| `ota-file <file.uf2>` | Inspect UF2 file locally (no device needed) |

### Console Commands

Connect via telnet or CLI interactive mode:

| Command | Description |
|---------|-------------|
| `ota` | Show OTA status (enabled, partitions, offsets) |
| `ota-enable [dur]` | Enable OTA server (e.g., `ota-enable 5m`) |
| `version` | Show firmware version, git SHA, build date |

## How It Works

### Update Flow

```
1. CLI sends "ota-enable" to console port 23 (auto-done by ota-push)
2. Device enables OTA server on port 4242 for 10 minutes
3. CLI connects to device on TCP port 4242
4. CLI sends "OTA\n"
5. Device responds "READY <max_size>\n"
6. CLI extracts raw binary from UF2 file
7. CLI sends chunks: <4-byte length><data> (4KB each)
8. Device erases flash sectors on-demand
9. Device writes chunks to inactive partition
10. Device responds "ACK <total_bytes>\n" per chunk
11. CLI sends "DONE <sha256_hex>\n"
12. Device verifies hash, responds "VERIFIED\n"
13. Device auto-disables OTA server (security)
14. Device calls rom_reboot(FLASH_UPDATE) to target partition
15. Bootrom boots new partition in TBYB mode
16. New firmware calls ConfirmPartition() within 16.7s
17. Update complete!
```

### Try-Before-You-Buy (TBYB)

The RP2350 bootrom implements TBYB as a safety mechanism:

- After FLASH_UPDATE reboot, bootrom starts a 16.7-second timer
- New firmware must call `ConfirmPartition()` (ROM `explicit_buy()`) before timeout
- If timeout expires or firmware crashes, bootrom automatically reverts to previous partition
- This ensures a bad update can never brick the device

The Bindicator calls `ConfirmPartition()` very early in boot (before WiFi init) to maximize the time window for successful confirmation.

### Security

The OTA server is **disabled by default** to minimize attack surface:

- OTA port 4242 only accepts connections when explicitly enabled
- Auto-disables after 10 minutes (configurable via `ota-enable <dur>`)
- Auto-disables after successful OTA transfer
- The `ota-push` CLI command automatically enables OTA before connecting

This means an attacker cannot push malicious firmware without first having console access (port 23) to enable OTA. Future enhancements may include firmware signing.

### Partition Detection

The firmware detects which partition it booted from using the ROM `get_sys_info()` function with the `BOOT_INFO` flag. This returns boot diagnostic information including the partition number.

```go
// Returns 0 for partition A, 1 for partition B
partition := ota.GetCurrentPartition()

// Target is always the OTHER partition
target := ota.GetTargetPartition()
```

## Initial Setup

### Prerequisites

- **picotool 2.2+** with partition support
- Device in BOOTSEL mode (hold button while connecting USB)

### First-Time Flash (with Partition Table)

```bash
# Create partition table UF2 from JSON definition
make partition-table

# Flash everything: partition table + firmware
make flash-full
```

Or manually:

```bash
# 1. Create partition table
picotool partition create partitions/bindicator.json partitions/pt.uf2

# 2. Load partition table
picotool load partitions/pt.uf2

# 3. Reboot to BOOTSEL
picotool reboot -u

# 4. Load firmware (with execute flag)
picotool load -x build.uf2

# 5. Reboot to run
picotool reboot
```

### Verify Partition Table

```bash
picotool partition info

# Expected output:
# un-partitioned_space :                          S(rw) NSBOOT(rw) NS(rw), id=00000000
# 0(A)       00002000->001f2000 S(rw) NSBOOT(rw) NS(rw), id=00000000, "Main A"
# 1(B w/ 0)  001f2000->003e2000 S(rw) NSBOOT(rw) NS(rw), id=00000000, "Main B"
```

## Technical Details

### OTA Protocol

```
Client → Device: "OTA\n"
Device → Client: "READY 2031616\n"              # Max firmware size in bytes

Client → Device: <4-byte LE length><chunk>      # Up to 4096 bytes per chunk
Device → Client: "ACK <total_bytes>\n"

... repeat for all chunks ...

Client → Device: "DONE <64-char-sha256-hex>\n"
Device → Client: "VERIFIED\n"
Device reboots to new partition
```

### Flash Operations

The OTA system uses **direct ROM flash functions** rather than TinyGo's `machine.Flash` API. This is necessary because:

- `machine.Flash.WriteAt()` adds `FlashDataStart()` to offsets
- `FlashDataStart()` points to the end of the current firmware (for app data storage)
- OTA needs to write to arbitrary flash locations (the other partition)

The `ota` package implements direct calls to:
- `flash_range_program()` - Write data to flash
- `flash_range_erase()` - Erase flash sectors

### ROM Functions Used

| Function | ROM Code | Purpose |
|----------|----------|---------|
| `get_sys_info` | 'GS' | Detect current boot partition |
| `explicit_buy` | 'EB' | Confirm partition (TBYB) |
| `reboot` | 'RB' | Trigger partition switch reboot |
| `flash_range_program` | 'RP' | Write to flash |
| `flash_range_erase` | 'RE' | Erase flash sectors |

### Partition Table Definition

`partitions/bindicator.json`:

```json
{
  "version": [1, 0],
  "unpartitioned": {
    "families": ["absolute"],
    "permissions": { "secure": "rw", "nonsecure": "rw", "bootloader": "rw" }
  },
  "partitions": [
    {
      "name": "Main A",
      "id": 0,
      "size": "1984K",
      "families": ["rp2350-arm-s", "rp2350-riscv"],
      "permissions": { "secure": "rw", "nonsecure": "rw", "bootloader": "rw" }
    },
    {
      "name": "Main B",
      "id": 0,
      "size": "1984K",
      "families": ["rp2350-arm-s", "rp2350-riscv"],
      "permissions": { "secure": "rw", "nonsecure": "rw", "bootloader": "rw" },
      "link": ["a", 0]
    }
  ]
}
```

The `"link": ["a", 0]` makes partition B an A/B partner of partition 0 (Main A), enabling automatic address translation by the bootrom.

## Troubleshooting

### OTA Transfer Fails

**Symptom**: Connection timeout or incomplete transfer

**Solutions**:
- Ensure device is on the network (`./bindicator-cli <ip> net`)
- Check firewall allows port 4242
- Verify firmware size < 1984KB

### Device Reverts After Update

**Symptom**: Device boots old firmware after ~16 seconds

**Causes**:
- New firmware crashes before calling `ConfirmPartition()`
- TBYB timeout (16.7s) expires

**Solutions**:
- Check new firmware runs correctly when flashed via picotool
- Ensure `ConfirmPartition()` is called early in boot
- Check serial output for crash messages

### Partition Not Detected

**Symptom**: `ota` command shows wrong partition

**Cause**: ROM `get_sys_info()` not returning expected data

**Solution**: Verify partition table is correctly flashed:
```bash
picotool partition info
```

### Flash Write Fails

**Symptom**: OTA completes but partition is empty

**Cause**: Using `machine.Flash` instead of direct ROM calls

**Solution**: The `ota` package must use `flash_range_program()` directly, not `machine.Flash.WriteAt()`. This was fixed in commit `ec9b4f1`.

## Implementation History

The OTA implementation required solving two key issues:

### Issue 1: Partition Detection (Fixed in `2b6b4ce`)

**Problem**: `GetCurrentPartition()` used QMI ATRANS register detection, which didn't work reliably.

**Solution**: Use ROM `get_sys_info()` with `BOOT_INFO` flag (same approach as Pico SDK).

### Issue 2: Flash Writes (Fixed in `ec9b4f1`)

**Problem**: TinyGo's `machine.Flash.WriteAt()` adds `FlashDataStart()` to offsets. This offset points to the end of the current firmware (for app data storage), not the start of flash. OTA writes were going to the wrong location.

**Solution**: Implement direct ROM flash function calls (`flash_range_program`, `flash_range_erase`) that take raw flash offsets.

## References

- [RP2350 Datasheet](https://datasheets.raspberrypi.com/rp2350/rp2350-datasheet.pdf)
  - Section 5.1.7: A/B Partitions
  - Section 5.1.17: Try Before You Buy (TBYB)
  - Section 5.1.19: Address Translation
  - Section 5.4.8.17: `get_sys_info()` ROM function
  - Section 5.4.8.24: `reboot()` ROM function
- [Pico SDK OTA Example](https://github.com/raspberrypi/pico-examples/tree/master/pico_w/wifi/ota_update)
- [picotool Partition Documentation](https://github.com/raspberrypi/picotool)
