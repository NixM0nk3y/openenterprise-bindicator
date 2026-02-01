//go:build tinygo

package main

import (
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"openenterprise/bindicator/config"
	"openenterprise/bindicator/credentials"
	"openenterprise/bindicator/ota"
	"openenterprise/bindicator/telemetry"
	"openenterprise/bindicator/version"

	"github.com/soypat/lneto/tcp"
	"github.com/soypat/lneto/x/xnet"
)

const (
	consolePort    = uint16(23) // Telnet port
	consoleBufSize = 1024
)

// Pre-allocated console buffers
var (
	consoleRxBuf [consoleBufSize]byte
	consoleTxBuf [consoleBufSize]byte
	consoleBuf   [consoleBufSize]byte
	startTime    time.Time
)

// Authentication state for brute-force protection
var (
	authFailures    int
	lastFailureTime time.Time
)

// Console commands
const (
	cmdHelp            = "help"
	cmdStatus          = "status"
	cmdRefresh         = "refresh"
	cmdTime            = "time"
	cmdJobs            = "jobs"
	cmdLeds            = "leds"
	cmdVersion         = "version"
	cmdNet             = "net"
	cmdWifi            = "wifi"
	cmdSleep           = "sleep"
	cmdLedGreen        = "led-green"
	cmdLedBlack        = "led-black"
	cmdLedBrown        = "led-brown"
	cmdNextJob         = "next"
	cmdOTA             = "ota"
	cmdOTAEnable       = "ota-enable"
	cmdReboot          = "reboot"
	cmdTelemetry       = "telemetry"
	cmdTelemetryFlush  = "telemetry-flush"
	cmdNTP             = "ntp"
	cmdNTPSync         = "ntp-sync"
)

// consoleServer runs a TCP debug console on port 23
func consoleServer(
	stack *xnet.StackAsync,
	logger *slog.Logger,
	refreshChan chan struct{},
) {
	// Recover from any panics to keep console server running
	defer func() {
		if r := recover(); r != nil {
			logger.Error("console:panic-recovered")
		}
	}()

	var conn tcp.Conn
	err := conn.Configure(tcp.ConnConfig{
		RxBuf:             consoleRxBuf[:],
		TxBuf:             consoleTxBuf[:],
		TxPacketQueueSize: 3,
	})
	if err != nil {
		logger.Error("console:configure-failed", slog.String("err", err.Error()))
		return
	}

	ourAddr := netip.AddrPortFrom(stack.Addr(), consolePort)
	logger.Info("console:listening", slog.String("addr", ourAddr.String()))

	for {
		// Always abort any previous state before listening
		conn.Abort()
		time.Sleep(100 * time.Millisecond)

		// Check lockout before accepting new connections
		if checkLockout() {
			lockout := getLockoutDuration()
			logger.Info("console:lockout", slog.Int("failures", authFailures), slog.Duration("remaining", lockout-time.Since(lastFailureTime)))
			time.Sleep(1 * time.Second)
			continue
		}

		// Listen for incoming connection
		err = stack.ListenTCP(&conn, consolePort)
		if err != nil {
			logger.Error("console:listen-failed", slog.String("err", err.Error()))
			time.Sleep(3 * time.Second)
			continue
		}

		// Wait for connection with timeout
		waitCount := 0
		for conn.State().IsPreestablished() && waitCount < 6000 {
			time.Sleep(10 * time.Millisecond)
			waitCount++
		}

		if !conn.State().IsSynchronized() {
			conn.Abort()
			continue
		}

		logger.Info("console:connected", slog.String("ip", formatRemoteIP(conn.RemoteAddr())))

		// Authenticate before allowing access
		if !authenticateConsole(&conn) {
			logger.Info("console:auth-failed", slog.Int("failures", authFailures))
			conn.Close()
			for i := 0; i < 10 && !conn.State().IsClosed(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			conn.Abort()
			continue
		}

		logger.Info("console:authenticated")

		// Send welcome message
		writeConsole(&conn, "Openenterprise Bindicator Debug Console\r\nType 'help' for commands\r\n> ")
		flushConsole(&conn)

		// Handle commands with recovery
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("console:session-panic")
				}
			}()
			handleConsoleSession(&conn, stack, logger, refreshChan)
		}()

		// Clean up connection
		conn.Close()
		for i := 0; i < 30 && !conn.State().IsClosed(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		conn.Abort()
		logger.Info("console:disconnected")
	}
}

// handleConsoleSession handles a single console session
func handleConsoleSession(conn *tcp.Conn, stack *xnet.StackAsync, logger *slog.Logger, refreshChan chan struct{}) {
	var cmdLen int
	var readBuf [64]byte // Separate read buffer
	var skipIAC int      // Bytes to skip for telnet IAC sequence

	for {
		// Check connection state (RxDataOpen detects CLOSE_WAIT from client disconnect)
		if conn.State().IsClosed() || conn.State().IsClosing() || !conn.State().RxDataOpen() {
			return
		}

		n, err := conn.Read(readBuf[:])
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			return
		}

		if n == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Copy to command buffer with bounds check
		gotNewline := false
		for i := 0; i < n && cmdLen < len(consoleBuf)-1; i++ {
			b := readBuf[i]

			// Skip remaining bytes from telnet IAC sequence
			if skipIAC > 0 {
				skipIAC--
				continue
			}

			// Handle telnet IAC (Interpret As Command) sequences
			// IAC = 0xFF, followed by command byte and possibly option byte
			if b == 0xFF {
				// Need to skip at least the command byte
				// WILL/WONT/DO/DONT (0xFB-0xFE) also have an option byte
				skipIAC = 2 // Skip command + option (safe default)
				continue
			}

			if b == '\n' || b == '\r' {
				// Skip consecutive CR/LF (telnet sends \r\n)
				if gotNewline {
					continue
				}
				gotNewline = true
				// Allow TCP stack time to process pending packets
				time.Sleep(10 * time.Millisecond)
				// Process command
				if cmdLen > 0 {
					processCommand(conn, stack, consoleBuf[:cmdLen], logger, refreshChan)
				}
				cmdLen = 0
				conn.Write([]byte("> "))
				conn.Flush()
				// Allow TCP stack time to process packets
				time.Sleep(50 * time.Millisecond)
			} else if b >= 32 && b < 127 { // Printable ASCII only
				consoleBuf[cmdLen] = b
				cmdLen++
				gotNewline = false
			}
		}

		// Prevent buffer overflow
		if cmdLen >= len(consoleBuf)-1 {
			cmdLen = 0
			writeConsole(conn, "\r\nLine too long\r\n> ")
			flushConsole(conn)
		}
	}
}

// processCommand handles a single console command
func processCommand(conn *tcp.Conn, stack *xnet.StackAsync, cmd []byte, logger *slog.Logger, refreshChan chan struct{}) {
	// Recover from panics to keep console running
	defer func() {
		if r := recover(); r != nil {
			logger.Error("console:command-panic")
		}
	}()

	switch {
	case bytesEqual(cmd, []byte(cmdHelp)):
		writeConsole(conn, "Commands: help version status net wifi time jobs next leds ota ntp\r\n")
		writeConsole(conn, "  refresh, sleep <dur>, ota-enable [dur], ntp-sync, reboot\r\n")
		writeConsole(conn, "  led-green, led-black, led-brown\r\n")
		writeConsole(conn, "  telemetry, telemetry-flush\r\n")

	case bytesEqual(cmd, []byte(cmdStatus)):
		if systemHealthy {
			writeConsole(conn, "Status: OK\r\n")
		} else {
			writeConsole(conn, "Status: UNHEALTHY (reset pending)\r\n")
		}
		writeConsole(conn, "Jobs loaded: ")
		writeInt(conn, jobCount)
		writeConsole(conn, "\r\n")
		writeConsole(conn, "Failures: ")
		writeInt(conn, consecutiveFailures)
		writeConsole(conn, "/")
		writeInt(conn, maxConsecutiveFailures)
		writeConsole(conn, "\r\n")
		writeConsole(conn, "Last refresh: ")
		if lastSuccessfulRefresh.IsZero() {
			writeConsole(conn, "never\r\n")
		} else {
			writeConsole(conn, lastSuccessfulRefresh.Format("15:04:05"))
			writeConsole(conn, " (")
			mins := int(time.Since(lastSuccessfulRefresh).Minutes())
			writeInt(conn, mins)
			writeConsole(conn, "m ago)\r\n")
		}

	case bytesEqual(cmd, []byte(cmdRefresh)):
		writeConsole(conn, "Triggering refresh...\r\n")
		select {
		case refreshChan <- struct{}{}:
			writeConsole(conn, "Refresh triggered\r\n")
		default:
			writeConsole(conn, "Refresh already pending\r\n")
		}

	case bytesEqual(cmd, []byte(cmdTime)):
		now := time.Now()
		writeConsole(conn, "Time: ")
		writeConsole(conn, now.Format("2006-01-02 15:04:05"))
		writeConsole(conn, " UTC\r\n")

	case bytesEqual(cmd, []byte(cmdJobs)):
		jobs := getJobs()
		if len(jobs) == 0 {
			writeConsole(conn, "No jobs loaded\r\n")
		} else {
			for i := 0; i < len(jobs); i++ {
				job := &jobs[i]
				writeInt(conn, int(job.Year))
				writeConsole(conn, "-")
				writeInt2(conn, int(job.Month))
				writeConsole(conn, "-")
				writeInt2(conn, int(job.Day))
				writeConsole(conn, " : ")
				writeBinType(conn, job.Bin)
				writeConsole(conn, "\r\n")
			}
		}

	case bytesEqual(cmd, []byte(cmdNextJob)):
		jobs := getJobs()
		now := time.Now()
		found := false
		for i := 0; i < len(jobs); i++ {
			job := &jobs[i]
			jobDate := time.Date(int(job.Year), time.Month(job.Month), int(job.Day), 0, 0, 0, 0, time.UTC)
			// Show job if it's today or in the future
			if !jobDate.Before(now.Truncate(24 * time.Hour)) {
				writeConsole(conn, "Next: ")
				writeInt(conn, int(job.Year))
				writeConsole(conn, "-")
				writeInt2(conn, int(job.Month))
				writeConsole(conn, "-")
				writeInt2(conn, int(job.Day))
				writeConsole(conn, " ")
				writeBinType(conn, job.Bin)
				// Calculate days until
				days := int(jobDate.Sub(now.Truncate(24*time.Hour)).Hours() / 24)
				writeConsole(conn, " (")
				if days == 0 {
					writeConsole(conn, "TODAY")
				} else if days == 1 {
					writeConsole(conn, "tomorrow")
				} else {
					writeInt(conn, days)
					writeConsole(conn, " days")
				}
				writeConsole(conn, ")\r\n")
				found = true
				break
			}
		}
		if !found {
			writeConsole(conn, "No upcoming jobs\r\n")
		}

	case bytesEqual(cmd, []byte(cmdLeds)):
		writeConsole(conn, "LED States:\r\n")
		writeConsole(conn, "  GREEN: ")
		writeBool(conn, ledState.green)
		writeConsole(conn, "\r\n  BLACK: ")
		writeBool(conn, ledState.black)
		writeConsole(conn, "\r\n  BROWN: ")
		writeBool(conn, ledState.brown)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdVersion)):
		writeConsole(conn, "Openenterprise Bindicator\r\n")
		writeConsole(conn, "  Version: ")
		writeConsole(conn, version.Version)
		writeConsole(conn, "\r\n  Git SHA: ")
		writeConsole(conn, version.GitSHA)
		writeConsole(conn, "\r\n  Built:   ")
		writeConsole(conn, version.BuildDate)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdNet)):
		writeConsole(conn, "Network Status:\r\n")
		writeConsole(conn, "  IP Address: ")
		writeConsole(conn, stack.Addr().String())
		writeConsole(conn, "\r\n  Console:    port ")
		writeInt(conn, int(consolePort))
		writeConsole(conn, "\r\n  Uptime:     ")
		writeUptime(conn)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdWifi)):
		writeConsole(conn, "WiFi Quality:\r\n")
		// Connection uptime
		writeConsole(conn, "  Connected:     ")
		if wifiStats.connectTime.IsZero() {
			writeConsole(conn, "unknown\r\n")
		} else {
			writeWifiUptime(conn, wifiStats.connectTime)
			writeConsole(conn, "\r\n")
		}
		// MQTT success rate
		total := wifiStats.mqttSuccessCount + wifiStats.mqttFailCount
		writeConsole(conn, "  MQTT success:  ")
		writeInt(conn, wifiStats.mqttSuccessCount)
		writeConsole(conn, "/")
		writeInt(conn, total)
		if total > 0 {
			pct := (wifiStats.mqttSuccessCount * 100) / total
			writeConsole(conn, " (")
			writeInt(conn, pct)
			writeConsole(conn, "%)")
		}
		writeConsole(conn, "\r\n")
		// Last success
		writeConsole(conn, "  Last success:  ")
		if wifiStats.lastMQTTSuccess.IsZero() {
			writeConsole(conn, "never\r\n")
		} else {
			writeConsole(conn, wifiStats.lastMQTTSuccess.Format("15:04:05"))
			writeConsole(conn, " (")
			mins := int(time.Since(wifiStats.lastMQTTSuccess).Minutes())
			writeInt(conn, mins)
			writeConsole(conn, "m ago)\r\n")
		}
		// Consecutive failures
		writeConsole(conn, "  Consecutive failures: ")
		writeInt(conn, consecutiveFailures)
		writeConsole(conn, "\r\n")

	case len(cmd) >= 5 && bytesEqual(cmd[:5], []byte(cmdSleep)):
		// Parse sleep duration: "sleep 30s", "sleep 1m", "sleep 0"
		if len(cmd) <= 6 {
			// Just "sleep" with no arg - show current
			writeConsole(conn, "Sleep override: ")
			if debugSleepDuration == 0 {
				writeConsole(conn, "off (using default 3h)\r\n")
			} else {
				writeInt(conn, int(debugSleepDuration.Seconds()))
				writeConsole(conn, "s\r\n")
			}
		} else {
			// Parse the duration argument
			arg := cmd[6:] // skip "sleep "
			dur := parseDuration(arg)
			debugSleepDuration = dur
			writeConsole(conn, "Sleep override set to: ")
			if dur == 0 {
				writeConsole(conn, "off (using default 3h)\r\n")
			} else {
				writeInt(conn, int(dur.Seconds()))
				writeConsole(conn, "s\r\n")
			}
		}

	case bytesEqual(cmd, []byte(cmdLedGreen)):
		ledState.green = !ledState.green
		setLED(BinGreen, ledState.green)
		writeConsole(conn, "GREEN LED: ")
		writeBool(conn, ledState.green)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdLedBlack)):
		ledState.black = !ledState.black
		setLED(BinBlack, ledState.black)
		writeConsole(conn, "BLACK LED: ")
		writeBool(conn, ledState.black)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdLedBrown)):
		ledState.brown = !ledState.brown
		setLED(BinBrown, ledState.brown)
		writeConsole(conn, "BROWN LED: ")
		writeBool(conn, ledState.brown)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdOTA)):
		currentPart := ota.GetCurrentPartition()
		targetPart := ota.GetTargetPartition()
		writeConsole(conn, "OTA Status:\r\n")
		writeConsole(conn, "  Server:            ")
		if OTAIsEnabled() {
			writeConsole(conn, "ENABLED (")
			remaining := OTATimeRemaining()
			writeInt(conn, int(remaining.Minutes()))
			writeConsole(conn, "m ")
			writeInt(conn, int(remaining.Seconds())%60)
			writeConsole(conn, "s remaining)\r\n")
		} else {
			writeConsole(conn, "disabled\r\n")
		}
		writeConsole(conn, "  Current partition: ")
		if currentPart == ota.PartitionA {
			writeConsole(conn, "A")
		} else {
			writeConsole(conn, "B")
		}
		writeConsole(conn, "\r\n  Target partition:  ")
		if targetPart == ota.PartitionA {
			writeConsole(conn, "A")
		} else {
			writeConsole(conn, "B")
		}
		writeConsole(conn, "\r\n  Partition A offset: 0x")
		writeHex(conn, ota.GetPartitionOffset(ota.PartitionA))
		writeConsole(conn, "\r\n  Partition B offset: 0x")
		writeHex(conn, ota.GetPartitionOffset(ota.PartitionB))
		writeConsole(conn, "\r\n  Max image size: ")
		writeInt(conn, int(ota.GetPartitionMaxSize()/1024))
		writeConsole(conn, " KB\r\n")

	case bytesEqual(cmd, []byte(cmdOTAEnable)) || hasPrefix(cmd, []byte(cmdOTAEnable+" ")):
		// Parse optional timeout (e.g., "ota-enable 5m")
		timeout := time.Duration(0) // Use default
		if len(cmd) > len(cmdOTAEnable)+1 {
			durationBytes := cmd[len(cmdOTAEnable)+1:]
			parsed := parseDuration(durationBytes)
			if parsed > 0 {
				timeout = parsed
			}
		}
		OTAEnable(timeout)
		writeConsole(conn, "OTA server enabled on port 4242\r\n")
		writeConsole(conn, "  Timeout: ")
		remaining := OTATimeRemaining()
		writeInt(conn, int(remaining.Minutes()))
		writeConsole(conn, " minutes\r\n")
		writeConsole(conn, "  Push updates with: bindicator-cli <ip> ota-push <file.uf2>\r\n")

	case bytesEqual(cmd, []byte(cmdReboot)):
		writeConsole(conn, "Rebooting device...\r\n")
		conn.Flush()
		time.Sleep(100 * time.Millisecond)
		ota.Reboot()

	case bytesEqual(cmd, []byte(cmdTelemetry)):
		enabled, qLogs, qMetrics, qSpans, sLogs, sMetrics, sSpans, errs, collector := telemetry.Status()
		writeConsole(conn, "Telemetry Status:\r\n")
		writeConsole(conn, "  Enabled:    ")
		if enabled {
			writeConsole(conn, "yes\r\n")
		} else {
			writeConsole(conn, "no\r\n")
		}
		writeConsole(conn, "  Collector:  ")
		writeConsole(conn, collector)
		writeConsole(conn, "\r\n  Queued:\r\n")
		writeConsole(conn, "    Logs:     ")
		writeInt(conn, qLogs)
		writeConsole(conn, "\r\n    Metrics:  ")
		writeInt(conn, qMetrics)
		writeConsole(conn, "\r\n    Spans:    ")
		writeInt(conn, qSpans)
		writeConsole(conn, "\r\n  Sent:\r\n")
		writeConsole(conn, "    Logs:     ")
		writeInt(conn, sLogs)
		writeConsole(conn, "\r\n    Metrics:  ")
		writeInt(conn, sMetrics)
		writeConsole(conn, "\r\n    Spans:    ")
		writeInt(conn, sSpans)
		writeConsole(conn, "\r\n  Errors:     ")
		writeInt(conn, errs)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdTelemetryFlush)):
		writeConsole(conn, "Flushing telemetry queues...\r\n")
		telemetry.Flush()
		writeConsole(conn, "Flush complete\r\n")

	case bytesEqual(cmd, []byte(cmdNTP)):
		writeConsole(conn, "NTP Status:\r\n")
		writeConsole(conn, "  Server:     ")
		writeConsole(conn, config.NTPServer())
		writeConsole(conn, "\r\n  Last sync:  ")
		if lastNTPSync.IsZero() {
			writeConsole(conn, "never\r\n")
		} else {
			writeConsole(conn, lastNTPSync.Format("15:04:05"))
			writeConsole(conn, " (")
			mins := int(time.Since(lastNTPSync).Minutes())
			writeInt(conn, mins)
			writeConsole(conn, "m ago)\r\n")
		}
		writeConsole(conn, "  Offset:     ")
		if ntpTimeOffset == 0 && lastNTPSync.IsZero() {
			writeConsole(conn, "unknown\r\n")
		} else {
			writeInt(conn, int(ntpTimeOffset.Milliseconds()))
			writeConsole(conn, "ms\r\n")
		}
		writeConsole(conn, "  Syncs:      ")
		writeInt(conn, ntpSyncCount)
		writeConsole(conn, "\r\n  Failures:   ")
		writeInt(conn, ntpFailCount)
		writeConsole(conn, "\r\n")

	case bytesEqual(cmd, []byte(cmdNTPSync)):
		writeConsole(conn, "Triggering NTP sync...\r\n")
		conn.Flush()
		offset, err := syncNTP(stack, dnsServers, logger)
		if err != nil {
			writeConsole(conn, "NTP sync failed: ")
			writeConsole(conn, err.Error())
			writeConsole(conn, "\r\n")
		} else {
			writeConsole(conn, "NTP sync complete\r\n")
			writeConsole(conn, "  Time:   ")
			writeConsole(conn, time.Now().Format("2006-01-02 15:04:05"))
			writeConsole(conn, " UTC\r\n")
			writeConsole(conn, "  Offset: ")
			writeInt(conn, int(offset.Milliseconds()))
			writeConsole(conn, "ms\r\n")
		}

	default:
		writeConsole(conn, "Unknown command: ")
		conn.Write(cmd)
		writeConsole(conn, "\r\nType 'help' for commands\r\n")
	}
	// Flush and allow TCP stack time to process packets
	conn.Flush()
	time.Sleep(50 * time.Millisecond)
}

// writeConsole writes a string to the console connection (no flush)
func writeConsole(conn *tcp.Conn, s string) {
	conn.Write([]byte(s))
}

// flushConsole flushes the console output
func flushConsole(conn *tcp.Conn) {
	conn.Flush()
}

// writeInt writes an integer to the console
func writeInt(conn *tcp.Conn, n int) {
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

// writeInt2 writes a 2-digit zero-padded integer
func writeInt2(conn *tcp.Conn, n int) {
	if n < 10 {
		conn.Write([]byte{'0', byte('0' + n)})
	} else {
		conn.Write([]byte{byte('0' + n/10), byte('0' + n%10)})
	}
}

// writeHex writes a uint32 as hexadecimal (no 0x prefix)
func writeHex(conn *tcp.Conn, n uint32) {
	const hexDigits = "0123456789abcdef"
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = hexDigits[n&0xf]
		n >>= 4
	}
	// Skip leading zeros but keep at least one digit
	start := 0
	for start < 7 && buf[start] == '0' {
		start++
	}
	conn.Write(buf[start:])
}

// writeBool writes ON/OFF for boolean
func writeBool(conn *tcp.Conn, b bool) {
	if b {
		conn.Write([]byte("ON"))
	} else {
		conn.Write([]byte("OFF"))
	}
}

// writeBinType writes the bin type name
func writeBinType(conn *tcp.Conn, bt BinType) {
	switch bt {
	case BinGreen:
		conn.Write([]byte("GREEN"))
	case BinBlack:
		conn.Write([]byte("BLACK"))
	case BinBrown:
		conn.Write([]byte("BROWN"))
	default:
		conn.Write([]byte("UNKNOWN"))
	}
}

// writeUptime writes the uptime in human-readable format
func writeUptime(conn *tcp.Conn) {
	if startTime.IsZero() {
		conn.Write([]byte("unknown"))
		return
	}
	d := time.Since(startTime)
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60

	writeInt(conn, hours)
	conn.Write([]byte("h "))
	writeInt(conn, mins)
	conn.Write([]byte("m "))
	writeInt(conn, secs)
	conn.Write([]byte("s"))
}

// writeWifiUptime writes the WiFi connection uptime
func writeWifiUptime(conn *tcp.Conn, since time.Time) {
	d := time.Since(since)
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60

	writeInt(conn, hours)
	conn.Write([]byte("h "))
	writeInt(conn, mins)
	conn.Write([]byte("m "))
	writeInt(conn, secs)
	conn.Write([]byte("s"))
}

// initConsole initializes the console module
func initConsole() {
	startTime = time.Now()
}

// getLockoutDuration returns the lockout duration based on failure count
func getLockoutDuration() time.Duration {
	switch {
	case authFailures >= 10:
		return 5 * time.Minute
	case authFailures >= 5:
		return 30 * time.Second
	case authFailures >= 3:
		return 5 * time.Second
	default:
		return 0
	}
}

// checkLockout checks if we're in a lockout period
// Returns true if connection should be rejected
func checkLockout() bool {
	lockout := getLockoutDuration()
	if lockout == 0 {
		return false
	}
	return time.Since(lastFailureTime) < lockout
}

// recordFailure records an authentication failure
func recordFailure() {
	authFailures++
	lastFailureTime = time.Now()
}

// resetFailures resets the failure counter on successful auth
func resetFailures() {
	authFailures = 0
}

// Telnet protocol bytes for echo control
var (
	telnetWillEcho = []byte{0xFF, 0xFB, 0x01} // IAC WILL ECHO - server handles echo (client stops)
	telnetWontEcho = []byte{0xFF, 0xFC, 0x01} // IAC WONT ECHO - server stops echo (client resumes)
)

// authenticateConsole prompts for password and verifies
// Returns true if authenticated, false otherwise
func authenticateConsole(conn *tcp.Conn) bool {
	// Disable client echo for password entry
	conn.Write(telnetWillEcho)
	writeConsole(conn, "Password: ")
	flushConsole(conn)

	// Read password with timeout
	var passBuf [64]byte
	var readBuf [64]byte
	var passLen int
	var skipIAC int // Bytes to skip for telnet IAC sequence
	deadline := time.Now().Add(10 * time.Second)

	// Helper to restore echo before returning
	restoreEcho := func() {
		conn.Write(telnetWontEcho)
		writeConsole(conn, "\r\n")
		flushConsole(conn)
	}

	for time.Now().Before(deadline) {
		if conn.State().IsClosed() || conn.State().IsClosing() || !conn.State().RxDataOpen() {
			restoreEcho()
			return false
		}

		n, err := conn.Read(readBuf[:])
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			restoreEcho()
			return false
		}

		if n == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Process received bytes
		for i := 0; i < n && passLen < len(passBuf)-1; i++ {
			b := readBuf[i]

			// Skip remaining bytes from telnet IAC sequence
			if skipIAC > 0 {
				skipIAC--
				continue
			}

			// Handle telnet IAC (Interpret As Command) sequences
			// IAC = 0xFF, followed by command byte and possibly option byte
			if b == 0xFF {
				skipIAC = 2 // Skip command + option
				continue
			}

			if b == '\n' || b == '\r' {
				// Got newline, verify password using constant-time comparison
				restoreEcho()
				password := passBuf[:passLen]
				expected := []byte(credentials.ConsolePassword())
				if subtle.ConstantTimeCompare(password, expected) == 1 {
					resetFailures()
					return true
				}
				recordFailure()
				return false
			} else if b >= 32 && b < 127 {
				passBuf[passLen] = b
				passLen++
			}
		}

		// Check for buffer overflow
		if passLen >= len(passBuf)-1 {
			restoreEcho()
			recordFailure()
			return false
		}
	}

	// Timeout
	restoreEcho()
	recordFailure()
	return false
}

// hasPrefix checks if cmd starts with prefix
func hasPrefix(cmd, prefix []byte) bool {
	if len(cmd) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if cmd[i] != prefix[i] {
			return false
		}
	}
	return true
}

// parseDuration parses simple duration strings like "30s", "5m", "1h", or "0"
func parseDuration(s []byte) time.Duration {
	if len(s) == 0 {
		return 0
	}

	// Parse the number part
	var num int
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		num = num*10 + int(s[i]-'0')
		i++
	}

	// If just "0" or no unit, treat as seconds
	if i >= len(s) {
		return time.Duration(num) * time.Second
	}

	// Parse unit
	switch s[i] {
	case 's', 'S':
		return time.Duration(num) * time.Second
	case 'm', 'M':
		return time.Duration(num) * time.Minute
	case 'h', 'H':
		return time.Duration(num) * time.Hour
	default:
		return time.Duration(num) * time.Second
	}
}

// formatRemoteIP formats a remote IP address as a string for logging
func formatRemoteIP(addr []byte) string {
	if len(addr) == 4 {
		// IPv4
		var buf [15]byte // max "255.255.255.255"
		pos := 0
		for i := 0; i < 4; i++ {
			if i > 0 {
				buf[pos] = '.'
				pos++
			}
			pos += writeIntToBuf(buf[pos:], int(addr[i]))
		}
		return string(buf[:pos])
	}
	return "unknown"
}

// writeIntToBuf writes an integer to a byte buffer, returns bytes written
func writeIntToBuf(buf []byte, n int) int {
	if n == 0 {
		buf[0] = '0'
		return 1
	}
	var digits [3]byte
	i := len(digits)
	for n > 0 && i > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	copy(buf, digits[i:])
	return len(digits) - i
}
