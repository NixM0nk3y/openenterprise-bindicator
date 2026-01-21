//go:build tinygo

// Package ota provides Over-The-Air firmware update support for RP2350.
// Uses the RP2350's native A/B partition system with TBYB (Try Before You Buy).
package ota

/*
#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>

// ============================================================================
// ROM Function Infrastructure (duplicated from TinyGo's machine_rp2350_rom.go)
// ============================================================================

// ROM table code macro - creates 16-bit code from two characters
#define ROM_TABLE_CODE(c1, c2) ((c1) | ((c2) << 8))

// ROM function codes
#define ROM_FUNC_REBOOT       ROM_TABLE_CODE('R', 'B')
#define ROM_FUNC_EXPLICIT_BUY ROM_TABLE_CODE('E', 'B')

// Bootrom constants
#define BOOTROM_FUNC_TABLE_OFFSET   0x14
#define BOOTROM_WELL_KNOWN_PTR_SIZE 2
#define BOOTROM_TABLE_LOOKUP_OFFSET (BOOTROM_FUNC_TABLE_OFFSET + BOOTROM_WELL_KNOWN_PTR_SIZE)

// ROM lookup flags
#define RT_FLAG_FUNC_ARM_SEC    0x0004
#define RT_FLAG_FUNC_ARM_NONSEC 0x0010

// Reboot type flags
#define REBOOT2_FLAG_REBOOT_TYPE_NORMAL       0x0
#define REBOOT2_FLAG_REBOOT_TYPE_BOOTSEL      0x2
#define REBOOT2_FLAG_REBOOT_TYPE_FLASH_UPDATE 0x4
#define REBOOT2_FLAG_NO_RETURN_ON_SUCCESS     0x100

// Function pointer types
typedef void *(*rom_table_lookup_fn)(uint32_t code, uint32_t mask);
typedef int (*rom_reboot_fn)(uint32_t flags, uint32_t delay_ms, uint32_t p0, uint32_t p1);
typedef int (*rom_explicit_buy_fn)(uint8_t *buffer, uint32_t buffer_size);

// Check if processor is in non-secure state
// TinyGo on RP2350 typically runs in Secure mode (no TrustZone configured)
__attribute__((always_inline))
static inline bool pico_processor_state_is_nonsecure(void) {
    // Try Secure mode first - TinyGo likely runs in Secure state
    return false;
}

// ROM function lookup (matches TinyGo's implementation pattern)
__attribute__((always_inline))
static void *rom_func_lookup_inline(uint32_t code) {
    rom_table_lookup_fn rom_table_lookup =
        (rom_table_lookup_fn)(uintptr_t)*(uint16_t*)(BOOTROM_TABLE_LOOKUP_OFFSET);
    if (pico_processor_state_is_nonsecure()) {
        return rom_table_lookup(code, RT_FLAG_FUNC_ARM_NONSEC);
    } else {
        return rom_table_lookup(code, RT_FLAG_FUNC_ARM_SEC);
    }
}

// ============================================================================
// OTA Functions
// ============================================================================

// Partition offsets (hardcoded for our 2-partition layout)
// Layout: PT (8KB) | Partition A (1984KB) | Partition B (1984KB) | Reserved
// Verified with: picotool partition info
//   0(A)       00002000->001f2000
//   1(B w/ 0)  001f2000->003e2000
//
// For flash operations (erase/write), use raw offsets from flash start.
// For reboot() API, bootrom expects XIP addresses (flash offset + 0x10000000).
#define XIP_BASE           0x10000000
#define PARTITION_A_OFFSET 0x2000      // 8KB after flash start (for flash ops)
#define PARTITION_B_OFFSET 0x1F2000    // 8KB + 1984KB (for flash ops)
#define PARTITION_MAX_SIZE 0x1F0000    // 1984KB = 2,031,616 bytes per partition

// XIP addresses for reboot API
#define PARTITION_A_XIP    (XIP_BASE + PARTITION_A_OFFSET)  // 0x10002000
#define PARTITION_B_XIP    (XIP_BASE + PARTITION_B_OFFSET)  // 0x101F2000

// ota_reboot performs a ROM reboot with specified flags
static int ota_reboot(uint32_t flags, uint32_t delay_ms, uint32_t p0, uint32_t p1) {
    rom_reboot_fn func = (rom_reboot_fn) rom_func_lookup_inline(ROM_FUNC_REBOOT);
    if (!func) return -1;
    return func(flags, delay_ms, p0, p1);
}

// ota_confirm_partition confirms the current partition (TBYB).
// Returns 0 on success, non-zero on failure.
// Must be called within 16.7s of boot or bootrom auto-reverts.
static int ota_confirm_partition(void) {
    rom_explicit_buy_fn func = (rom_explicit_buy_fn) rom_func_lookup_inline(ROM_FUNC_EXPLICIT_BUY);
    if (!func) return -1;
    uint32_t workarea[64];  // SDK recommends 256 bytes for workarea, aligned to 4 bytes
    return func((uint8_t*)workarea, sizeof(workarea));
}

// ota_get_last_reboot_result stores the last reboot attempt result
static int last_reboot_result = 0;

// ota_reboot_to_partition triggers a reboot into the specified partition.
// Matches Pico SDK reference implementation:
// - Uses XIP address (code_start_addr + XIP_BASE)
// - Uses 1000ms delay
// Per RP2350 datasheet 5.4.8.24: For REBOOT_TYPE_FLASH_UPDATE, p0 must be
// the flash address of the updated region (update_base).
static void ota_reboot_to_partition(int partition) {
    // Calculate XIP address (matching Pico SDK: code_start_addr + XIP_BASE)
    uint32_t flash_offset = (partition == 0) ? PARTITION_A_OFFSET : PARTITION_B_OFFSET;
    uint32_t xip_addr = XIP_BASE + flash_offset;

    // Per pico/bootrom.h documentation:
    // - p0: flash region start address (partition receives preferential treatment)
    // - p1: region size (word-aligned) - REQUIRED for FLASH_UPDATE!
    // NOTE: Pico SDK uses 0 for p1. Trying 0 to match reference implementation.
    last_reboot_result = ota_reboot(
        REBOOT2_FLAG_REBOOT_TYPE_FLASH_UPDATE | REBOOT2_FLAG_NO_RETURN_ON_SUCCESS,
        1000,              // delay_ms
        xip_addr,          // p0: partition XIP address
        0                  // p1: partition size (0 = auto/default?)
    );

    if (last_reboot_result == 0) {
        // Success - busy wait for reboot
        for (volatile uint32_t i = 0; i < 20000000; i++) { }
        // Should not reach here
        while(1) { __asm__("wfi"); }
    }
    // Fall through on error - caller will check via GetRebootResult()
}

static int ota_get_reboot_result(void) {
    return last_reboot_result;
}

// ota_reboot_normal performs a normal system reboot using watchdog.
// This is more reliable than ROM reboot on RP2350.
static void ota_reboot_normal(void) {
    // RP2350 watchdog registers (from datasheet section 12.9)
    // NOTE: 0x400d8000, NOT 0x40058000 (which is PLL_USB)
    #define WATCHDOG_BASE 0x400d8000
    #define WATCHDOG_CTRL (WATCHDOG_BASE + 0x00)

    // CTRL register bits:
    // Bit 31 = TRIGGER - forces immediate watchdog reset
    // Bit 30 = ENABLE
    #define WATCHDOG_CTRL_TRIGGER (1u << 31)

    // Force immediate watchdog reset using TRIGGER bit
    *(volatile uint32_t*)WATCHDOG_CTRL = WATCHDOG_CTRL_TRIGGER;

    // Should not reach here
    while(1) { __asm__("nop"); }
}

// ota_get_partition_offset returns the flash offset for a partition.
static uint32_t ota_get_partition_offset(int partition) {
    return (partition == 0) ? PARTITION_A_OFFSET : PARTITION_B_OFFSET;
}

// ota_get_partition_xip_addr returns the XIP address for a partition.
static uint32_t ota_get_partition_xip_addr(int partition) {
    return XIP_BASE + ota_get_partition_offset(partition);
}

// ota_get_partition_max_size returns the maximum size for a partition.
static uint32_t ota_get_partition_max_size(void) {
    return PARTITION_MAX_SIZE;
}

// ============================================================================
// Current Partition Detection (using ROM get_sys_info)
// ============================================================================

// ROM function code for get_sys_info
#define ROM_FUNC_GET_SYS_INFO ROM_TABLE_CODE('G', 'S')

// Flag for BOOT_INFO
#define SYS_INFO_BOOT_INFO 0x0040

typedef int (*rom_get_sys_info_fn)(uint32_t *out_buffer, uint32_t out_buffer_word_size, uint32_t flags);

// ota_get_current_partition returns which partition we booted from.
// Uses ROM get_sys_info() with BOOT_INFO flag - same approach as Pico SDK.
// Per RP2350 datasheet 5.4.8.17: Word 1 is 0xttppbbdd where pp = boot partition
static int ota_get_current_partition(void) {
    rom_get_sys_info_fn func = (rom_get_sys_info_fn) rom_func_lookup_inline(ROM_FUNC_GET_SYS_INFO);
    if (!func) return 0;  // Default to A on error

    uint32_t buffer[5];
    int ret = func(buffer, 5, SYS_INFO_BOOT_INFO);
    if (ret < 0) return 0;  // Default to A on error

    // Check that BOOT_INFO flag is supported
    if (!(buffer[0] & SYS_INFO_BOOT_INFO)) return 0;

    // Extract partition from byte 2 of word 1 (the 'pp' byte in 0xttppbbdd)
    uint8_t partition = (buffer[1] >> 16) & 0xFF;

    // 0xFF means "none" (e.g., direct flash boot without partition table)
    if (partition == 0xFF) return 0;

    return (int)partition;
}

// ============================================================================
// Direct Flash Operations (bypasses TinyGo's machine.Flash which uses wrong offsets)
// Adapted from TinyGo's machine_rp2350_rom.go flash implementation
// ============================================================================

// ROM function codes for flash operations
#define ROM_FUNC_CONNECT_INTERNAL_FLASH ROM_TABLE_CODE('I', 'F')
#define ROM_FUNC_FLASH_EXIT_XIP         ROM_TABLE_CODE('E', 'X')
#define ROM_FUNC_FLASH_RANGE_ERASE      ROM_TABLE_CODE('R', 'E')
#define ROM_FUNC_FLASH_RANGE_PROGRAM    ROM_TABLE_CODE('R', 'P')
#define ROM_FUNC_FLASH_FLUSH_CACHE      ROM_TABLE_CODE('F', 'C')

// Flash constants
#define FLASH_SECTOR_SIZE      4096
#define FLASH_SECTOR_ERASE_CMD 0x20  // 4KB sector erase

// Function pointer types for flash operations
typedef void (*flash_connect_internal_fn)(void);
typedef void (*flash_exit_xip_fn)(void);
typedef void (*flash_range_erase_fn)(uint32_t addr, size_t count, uint32_t block_size, uint8_t block_cmd);
typedef void (*flash_range_program_fn)(uint32_t addr, const uint8_t *data, size_t count);
typedef void (*flash_flush_cache_fn)(void);

// ota_flash_write writes data to flash at the given raw offset.
// offset is raw flash offset (e.g., 0x1F2000 for partition B)
// This bypasses TinyGo's machine.Flash which adds FlashDataStart().
// Simplified implementation - relies on TinyGo having set up XIP/boot2 correctly.
static void ota_flash_write(uint32_t offset, const uint8_t *data, uint32_t len) {
    flash_connect_internal_fn connect = (flash_connect_internal_fn)rom_func_lookup_inline(ROM_FUNC_CONNECT_INTERNAL_FLASH);
    flash_exit_xip_fn exit_xip = (flash_exit_xip_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_EXIT_XIP);
    flash_range_program_fn program = (flash_range_program_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_RANGE_PROGRAM);
    flash_flush_cache_fn flush = (flash_flush_cache_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_FLUSH_CACHE);

    if (!connect || !exit_xip || !program || !flush) return;

    // Disable interrupts during flash operation
    uint32_t status;
    __asm__ volatile ("mrs %0, primask" : "=r" (status));
    __asm__ volatile ("cpsid i");

    connect();
    exit_xip();
    program(offset, data, len);
    flush();

    // Re-enable interrupts
    __asm__ volatile ("msr primask, %0" : : "r" (status));
}

// ota_flash_erase erases flash sectors at the given raw offset.
// offset is raw flash offset, count is number of BYTES to erase (must be multiple of 4096)
static void ota_flash_erase(uint32_t offset, uint32_t count) {
    flash_connect_internal_fn connect = (flash_connect_internal_fn)rom_func_lookup_inline(ROM_FUNC_CONNECT_INTERNAL_FLASH);
    flash_exit_xip_fn exit_xip = (flash_exit_xip_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_EXIT_XIP);
    flash_range_erase_fn erase = (flash_range_erase_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_RANGE_ERASE);
    flash_flush_cache_fn flush = (flash_flush_cache_fn)rom_func_lookup_inline(ROM_FUNC_FLASH_FLUSH_CACHE);

    if (!connect || !exit_xip || !erase || !flush) return;

    // Disable interrupts during flash operation
    uint32_t status;
    __asm__ volatile ("mrs %0, primask" : "=r" (status));
    __asm__ volatile ("cpsid i");

    connect();
    exit_xip();
    erase(offset, count, FLASH_SECTOR_SIZE, FLASH_SECTOR_ERASE_CMD);
    flush();

    // Re-enable interrupts
    __asm__ volatile ("msr primask, %0" : : "r" (status));
}
*/
import "C"

import (
	"errors"
)

// Partition constants
const (
	PartitionA = 0
	PartitionB = 1

	// Flash constants
	SectorSize = 4096 // 4KB erase block
	PageSize   = 256  // 256B write block
)

// Errors
var (
	ErrConfirmFailed    = errors.New("ota: partition confirm failed")
	ErrRebootFailed     = errors.New("ota: reboot failed")
	ErrImageTooLarge    = errors.New("ota: image too large for partition")
	ErrFlashWriteFailed = errors.New("ota: flash write failed")
	ErrFlashEraseFailed = errors.New("ota: flash erase failed")
)

// ConfirmPartition confirms the current partition (TBYB).
// Must be called within 16.7s of boot or bootrom auto-reverts to previous partition.
// Safe to call even if TBYB is not pending (returns success).
func ConfirmPartition() error {
	ret := C.ota_confirm_partition()
	if ret != 0 {
		return ErrConfirmFailed
	}
	return nil
}

// ConfirmPartitionWithCode confirms partition and returns the raw ROM return code.
// 0 = success, negative = error code from ROM.
func ConfirmPartitionWithCode() int {
	return int(C.ota_confirm_partition())
}

// RebootToPartition triggers a reboot into the specified partition.
// Calls WiFi shutdown callback first if registered (like Pico SDK's cyw43_arch_deinit).
// Does not return on success.
func RebootToPartition(partition int) {
	// Call WiFi shutdown if registered
	if wifiShutdownFunc != nil {
		wifiShutdownFunc()
	}
	C.ota_reboot_to_partition(C.int(partition))
}

// Reboot performs a normal system reboot.
// Does not return on success.
func Reboot() {
	// Call WiFi shutdown if registered
	if wifiShutdownFunc != nil {
		wifiShutdownFunc()
	}
	C.ota_reboot_normal()
}

// GetPartitionOffset returns the flash offset for a partition.
func GetPartitionOffset(partition int) uint32 {
	return uint32(C.ota_get_partition_offset(C.int(partition)))
}

// GetPartitionXIPAddr returns the XIP address for a partition.
func GetPartitionXIPAddr(partition int) uint32 {
	return uint32(C.ota_get_partition_xip_addr(C.int(partition)))
}

// GetRebootResult returns the result of the last reboot attempt.
// 0 = success, negative = error code
func GetRebootResult() int {
	return int(C.ota_get_reboot_result())
}

// GetPartitionMaxSize returns the maximum firmware size for a partition.
func GetPartitionMaxSize() uint32 {
	return uint32(C.ota_get_partition_max_size())
}

// GetCurrentPartition returns which partition we booted from (0=A, 1=B).
// Detection: Uses ROM get_sys_info() with BOOT_INFO flag (same as Pico SDK).
// Per RP2350 datasheet 5.4.8.17: Word 1 is 0xttppbbdd where pp = boot partition.
func GetCurrentPartition() int {
	return int(C.ota_get_current_partition())
}

// GetTargetPartition returns the inactive partition (for writing updates).
// Returns the opposite of the current boot partition.
func GetTargetPartition() int {
	if GetCurrentPartition() == PartitionA {
		return PartitionB
	}
	return PartitionA
}

// wifiShutdownFunc is called before reboot to cleanly shut down WiFi
var wifiShutdownFunc func()

// SetWiFiShutdown registers a function to call before reboot.
// This should shut down WiFi cleanly (like Pico SDK's cyw43_arch_deinit).
func SetWiFiShutdown(fn func()) {
	wifiShutdownFunc = fn
}

// WriteChunk writes a chunk of firmware data to flash.
// offset is the raw byte offset from flash start (not XIP address).
// Uses direct ROM flash functions to bypass TinyGo's machine.Flash
// which incorrectly adds FlashDataStart() to the offset.
func WriteChunk(offset uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	C.ota_flash_write(C.uint32_t(offset), (*C.uint8_t)(&data[0]), C.uint32_t(len(data)))
	return nil
}

// EraseSector erases a 4KB sector at the given offset.
// offset is the raw byte offset from flash start.
// Uses direct ROM flash functions to bypass TinyGo's machine.Flash.
func EraseSector(offset uint32) error {
	C.ota_flash_erase(C.uint32_t(offset), C.uint32_t(SectorSize))
	return nil
}

// ErasePartition erases an entire partition.
// This may take several seconds.
// Uses direct ROM flash functions to bypass TinyGo's machine.Flash.
func ErasePartition(partition int) error {
	offset := GetPartitionOffset(partition)
	maxSize := GetPartitionMaxSize()
	C.ota_flash_erase(C.uint32_t(offset), C.uint32_t(maxSize))
	return nil
}
