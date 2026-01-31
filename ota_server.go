//go:build tinygo

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"openenterprise/bindicator/ota"
	"openenterprise/bindicator/telemetry"

	"github.com/soypat/lneto/tcp"
	"github.com/soypat/lneto/x/xnet"
)

const (
	otaPort            = uint16(4242)
	otaBufSize         = 4096 + 64       // 4KB chunk + header room
	otaMaxFwSize       = 1984 * 1024     // Max firmware size (1984KB)
	otaDefaultTimeout  = 10 * time.Minute // Auto-disable after 10 minutes
)

// Pre-allocated OTA buffers
var (
	otaRxBuf [otaBufSize]byte
	otaTxBuf [512]byte
	otaChunk [otaBufSize]byte
)

// OTA server state (protected by mutex for thread-safety)
var (
	otaMu          sync.Mutex
	otaEnabled     bool
	otaEnabledAt   time.Time
	otaTimeout     time.Duration
	otaStack       *xnet.StackAsync
	otaLogger      *slog.Logger
	otaServerReady bool // Set when otaServerLoop is running
)

// OTAEnable enables the OTA server for the specified duration.
// If duration is 0, uses the default timeout.
func OTAEnable(timeout time.Duration) {
	otaMu.Lock()
	defer otaMu.Unlock()

	if timeout == 0 {
		timeout = otaDefaultTimeout
	}
	otaEnabled = true
	otaEnabledAt = time.Now()
	otaTimeout = timeout

	if otaLogger != nil {
		otaLogger.Info("ota:enabled", slog.String("timeout", timeout.String()))
	}
}

// OTADisable disables the OTA server.
func OTADisable() {
	otaMu.Lock()
	defer otaMu.Unlock()

	otaEnabled = false
	if otaLogger != nil {
		otaLogger.Info("ota:disabled")
	}
}

// OTAIsEnabled returns true if OTA server is currently enabled.
func OTAIsEnabled() bool {
	otaMu.Lock()
	defer otaMu.Unlock()

	if !otaEnabled {
		return false
	}

	// Check if timeout has expired
	if time.Since(otaEnabledAt) > otaTimeout {
		otaEnabled = false
		if otaLogger != nil {
			otaLogger.Info("ota:timeout-expired")
		}
		return false
	}

	return true
}

// OTATimeRemaining returns the time remaining before OTA auto-disables.
// Returns 0 if OTA is disabled.
func OTATimeRemaining() time.Duration {
	otaMu.Lock()
	defer otaMu.Unlock()

	if !otaEnabled {
		return 0
	}

	remaining := otaTimeout - time.Since(otaEnabledAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// otaServerInit initializes the OTA server (must be called from main).
// The server starts in disabled state - use OTAEnable() to enable.
func otaServerInit(stack *xnet.StackAsync, logger *slog.Logger) {
	otaMu.Lock()
	otaStack = stack
	otaLogger = logger
	otaMu.Unlock()

	go otaServerLoop()
}

// otaServerLoop runs the OTA server loop. Only accepts connections when enabled.
func otaServerLoop() {
	otaMu.Lock()
	stack := otaStack
	logger := otaLogger
	otaServerReady = true
	otaMu.Unlock()

	// Recover from panics
	defer func() {
		if r := recover(); r != nil {
			logger.Error("ota:panic-recovered")
		}
	}()

	var conn tcp.Conn
	err := conn.Configure(tcp.ConnConfig{
		RxBuf:             otaRxBuf[:],
		TxBuf:             otaTxBuf[:],
		TxPacketQueueSize: 2,
	})
	if err != nil {
		logger.Error("ota:configure-failed", slog.String("err", err.Error()))
		return
	}

	logger.Info("ota:ready", slog.Int("port", int(otaPort)))

	for {
		// Wait until OTA is enabled
		for !OTAIsEnabled() {
			time.Sleep(500 * time.Millisecond)
		}

		logger.Info("ota:listening", slog.Int("port", int(otaPort)))

		// Abort any previous state
		conn.Abort()
		time.Sleep(100 * time.Millisecond)

		// Listen for incoming connection
		err = stack.ListenTCP(&conn, otaPort)
		if err != nil {
			logger.Error("ota:listen-failed", slog.String("err", err.Error()))
			time.Sleep(3 * time.Second)
			continue
		}

		// Wait for connection with OTA enabled check
		waitCount := 0
		for conn.State().IsPreestablished() && waitCount < 6000 && OTAIsEnabled() {
			time.Sleep(10 * time.Millisecond)
			waitCount++
		}

		// Check if OTA was disabled while waiting
		if !OTAIsEnabled() {
			conn.Abort()
			logger.Info("ota:disabled-while-waiting")
			continue
		}

		if !conn.State().IsSynchronized() {
			conn.Abort()
			continue
		}

		logger.Info("ota:connected", slog.String("ip", formatRemoteIP(conn.RemoteAddr())))

		// Handle OTA session
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("ota:session-panic")
				}
			}()
			handleOTASession(&conn, logger)
		}()

		// Clean up
		conn.Close()
		for i := 0; i < 30 && !conn.State().IsClosed(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		conn.Abort()
		logger.Info("ota:disconnected")

		// Disable OTA after successful session (security: minimize window)
		OTADisable()
	}
}

// handleOTASession handles a single OTA update session
func handleOTASession(conn *tcp.Conn, logger *slog.Logger) {
	logger.Warn("ota:pausing-background-tasks")

	// Pause telemetry and bindicator during OTA to avoid network contention
	// Note: We don't flush here to avoid stability issues - flush happens just before reboot
	telemetry.Pause()
	SetBindicatorPaused(true)
	defer func() {
		// Resume if we return without rebooting (error case)
		SetBindicatorPaused(false)
		telemetry.Resume()
		logger.Warn("ota:resuming-background-tasks")
		telemetry.Flush()
	}()

	var readBuf [128]byte // Large enough for DONE + 64-char hash + newline

	// Wait for "OTA\n" initiation
	n, err := readWithTimeout(conn, readBuf[:], 10*time.Second)
	if err != nil || n < 3 {
		logger.Error("ota:no-init")
		return
	}

	if string(readBuf[:3]) != "OTA" {
		logger.Error("ota:bad-init", slog.String("got", string(readBuf[:n])))
		return
	}

	// Send READY with max size
	writeOTA(conn, "READY ")
	writeOTAInt(conn, otaMaxFwSize)
	writeOTA(conn, "\n")
	flushOTA(conn)

	// Give network stack time to send
	time.Sleep(100 * time.Millisecond)

	logger.Info("ota:ready", slog.Int("max_size", otaMaxFwSize))

	// Prepare for receiving firmware
	targetPartition := ota.GetTargetPartition()
	partitionOffset := ota.GetPartitionOffset(targetPartition)

	logger.Info("ota:target",
		slog.Int("partition", targetPartition),
		slog.String("offset", formatHex(partitionOffset)),
	)

	// Track which sectors have been erased (erase on-demand to avoid blocking)
	var erasedSectors [512]bool // Supports up to 2MB (512 x 4KB sectors)

	// Receive chunks
	var totalBytes uint32
	var hasher = sha256.New()
	chunkNum := 0

	for {
		// Feed watchdog during long operations
		feedWatchdogIfHealthy()

		// Read chunk header (4 bytes length) or DONE command
		err := readExactly(conn, readBuf[:4], 30*time.Second)
		if err != nil {
			logger.Error("ota:read-timeout", slog.String("err", err.Error()))
			return
		}

		// Check for DONE command
		if string(readBuf[:4]) == "DONE" {
			// Read rest of DONE line (space + hash + newline)
			n2, _ := readWithTimeout(conn, readBuf[4:], 2*time.Second)
			fullCmd := string(readBuf[:4+n2])

			// Parse expected hash (DONE <64-char-hex>\n)
			expectedHash := ""
			if len(fullCmd) > 5 {
				expectedHash = trimSpace(fullCmd[5:])
			}

			// Verify hash
			actualHash := hasher.Sum(nil)
			actualHashHex := formatHashHex(actualHash)

			logger.Info("ota:verifying",
				slog.Int("bytes", int(totalBytes)),
				slog.Int("expected_len", len(expectedHash)),
				slog.Int("actual_len", len(actualHashHex)),
			)
			logger.Info("ota:hash-expected", slog.String("hash", expectedHash))
			logger.Info("ota:hash-actual", slog.String("hash", actualHashHex))

			if expectedHash != "" && expectedHash != actualHashHex {
				logger.Error("ota:hash-mismatch")
				writeOTA(conn, "ERROR hash mismatch\n")
				flushOTA(conn)
				return
			}

			writeOTA(conn, "VERIFIED\n")
			flushOTA(conn)

			logger.Info("ota:complete",
				slog.Int("bytes", int(totalBytes)),
				slog.Int("chunks", chunkNum),
			)

			// Small delay to ensure response is sent
			time.Sleep(500 * time.Millisecond)

			// Reboot to new partition
			flashOffset := ota.GetPartitionOffset(targetPartition)
			xipAddr := ota.GetPartitionXIPAddr(targetPartition)
			logger.Info("ota:rebooting",
				slog.Int("partition", targetPartition),
				slog.String("flash_offset", formatHex(flashOffset)),
				slog.String("xip_addr", formatHex(xipAddr)),
			)

			// Flush telemetry just before reboot to capture validation messages
			telemetry.Resume() // Resume briefly to allow flush
			telemetry.Flush()
			time.Sleep(3000 * time.Millisecond) // Allow network stack to complete

			ota.RebootToPartition(targetPartition)
			// If we get here, reboot failed
			errCode := ota.GetRebootResult()
			logger.Error("ota:reboot-failed", slog.Int("error_code", errCode))
			return
		}

		// Parse chunk length
		chunkLen := binary.LittleEndian.Uint32(readBuf[:4])
		if chunkLen > uint32(len(otaChunk)) {
			logger.Error("ota:chunk-too-large", slog.Int("size", int(chunkLen)))
			writeOTA(conn, "ERROR chunk too large\n")
			flushOTA(conn)
			return
		}

		if totalBytes+chunkLen > otaMaxFwSize {
			logger.Error("ota:firmware-too-large")
			writeOTA(conn, "ERROR firmware too large\n")
			flushOTA(conn)
			return
		}

		// Read chunk data using readExactly
		err = readExactly(conn, otaChunk[:chunkLen], 30*time.Second)
		if err != nil {
			logger.Error("ota:chunk-read-failed",
				slog.Int("chunk", chunkNum),
				slog.Int("expected", int(chunkLen)),
				slog.String("err", err.Error()),
			)
			return
		}

		// Update hash
		hasher.Write(otaChunk[:chunkLen])

		// Calculate flash offset and which sectors need erasing
		flashOffset := partitionOffset + totalBytes

		// Log chunk receive with hash info for first few chunks (helps debug)
		if chunkNum < 3 {
			// Show first 8 bytes of chunk for verification
			logger.Info("ota:chunk-debug",
				slog.Int("chunk", chunkNum),
				slog.Int("size", int(chunkLen)),
				slog.String("first8", formatHashHex(otaChunk[:8])),
			)
		} else if chunkNum%20 == 0 {
			logger.Debug("ota:chunk-received",
				slog.Int("chunk", chunkNum),
				slog.Int("size", int(chunkLen)),
				slog.Int("total", int(totalBytes)),
			)
		}

		// Erase sectors on-demand (4KB each) to avoid blocking for too long
		startSector := totalBytes / ota.SectorSize
		endSector := (totalBytes + chunkLen - 1) / ota.SectorSize
		for sector := startSector; sector <= endSector; sector++ {
			if sector < uint32(len(erasedSectors)) && !erasedSectors[sector] {
				sectorOffset := partitionOffset + (sector * ota.SectorSize)

				// Log erase operation
				if sector < 5 || sector%50 == 0 {
					logger.Debug("ota:erasing-sector",
						slog.Int("sector", int(sector)),
						slog.String("offset", formatHex(sectorOffset)),
					)
				}

				feedWatchdogIfHealthy() // Feed watchdog before each erase
				err = ota.EraseSector(sectorOffset)
				if err != nil {
					logger.Error("ota:erase-failed",
						slog.Int("sector", int(sector)),
						slog.String("err", err.Error()),
					)
					writeOTA(conn, "ERROR erase failed\n")
					flushOTA(conn)
					return
				}
				erasedSectors[sector] = true

				// Give network stack time after erase
				time.Sleep(10 * time.Millisecond)
				for i := 0; i < 10; i++ {
					runtime.Gosched()
				}
			}
		}

		// Write to flash
		feedWatchdogIfHealthy() // Feed watchdog before write
		err = ota.WriteChunk(flashOffset, otaChunk[:chunkLen])
		if err != nil {
			logger.Error("ota:write-failed",
				slog.Int("chunk", chunkNum),
				slog.String("err", err.Error()),
			)
			writeOTA(conn, "ERROR write failed\n")
			flushOTA(conn)
			return
		}

		totalBytes += chunkLen
		chunkNum++

		// Send ACK
		writeOTA(conn, "ACK ")
		writeOTAInt(conn, int(totalBytes))
		writeOTA(conn, "\n")
		flushOTA(conn)

		// Give network stack time to send ACK and receive next chunk
		time.Sleep(20 * time.Millisecond)
		for i := 0; i < 10; i++ {
			runtime.Gosched()
		}
	}
}

// readWithTimeout reads from connection with timeout (returns on first data)
func readWithTimeout(conn *tcp.Conn, buf []byte, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	totalRead := 0

	for time.Now().Before(deadline) {
		if conn.State().IsClosed() || conn.State().IsClosing() {
			return totalRead, io.EOF
		}

		n, err := conn.Read(buf[totalRead:])
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			return totalRead, err
		}

		if n > 0 {
			totalRead += n
			return totalRead, nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	return totalRead, errors.New("timeout")
}

// readExactly reads exactly n bytes from connection with timeout
func readExactly(conn *tcp.Conn, buf []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	totalRead := 0
	needed := len(buf)

	for totalRead < needed && time.Now().Before(deadline) {
		if conn.State().IsClosed() || conn.State().IsClosing() {
			return io.EOF
		}

		n, err := conn.Read(buf[totalRead:])
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			return err
		}

		if n > 0 {
			totalRead += n
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if totalRead < needed {
		return errors.New("timeout")
	}
	return nil
}

// writeOTA writes a string to the OTA connection
func writeOTA(conn *tcp.Conn, s string) {
	conn.Write([]byte(s))
}

// writeOTAInt writes an integer to the OTA connection
func writeOTAInt(conn *tcp.Conn, n int) {
	if n == 0 {
		conn.Write([]byte{'0'})
		return
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	conn.Write(buf[i:])
}

// flushOTA flushes the OTA connection
func flushOTA(conn *tcp.Conn) {
	conn.Flush()
	for i := 0; i < 5; i++ {
		runtime.Gosched()
	}
}

// formatHex formats a uint32 as hex string
func formatHex(n uint32) string {
	const hexDigits = "0123456789abcdef"
	var buf [10]byte
	buf[0] = '0'
	buf[1] = 'x'
	for i := 9; i >= 2; i-- {
		buf[i] = hexDigits[n&0xf]
		n >>= 4
	}
	return string(buf[:])
}

// formatHashHex formats a hash as hex string
func formatHashHex(hash []byte) string {
	const hexDigits = "0123456789abcdef"
	result := make([]byte, len(hash)*2)
	for i, b := range hash {
		result[i*2] = hexDigits[b>>4]
		result[i*2+1] = hexDigits[b&0xf]
	}
	return string(result)
}

// trimSpace trims whitespace from string
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// truncate truncates a string to max length
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
