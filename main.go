//go:build tinygo

package main

// WARNING: default -scheduler=cores unsupported, compile with -scheduler=tasks set!

import (
	"log/slog"
	"machine"
	"net/netip"
	"runtime"
	"time"

	"openenterprise/bindicator/config"
	"openenterprise/bindicator/credentials"
	"openenterprise/bindicator/ota"
	"openenterprise/bindicator/telemetry"
	"openenterprise/bindicator/version"

	"github.com/soypat/cyw43439"
	"github.com/soypat/cyw43439/examples/cywnet"
	"github.com/soypat/lneto/x/xnet"
)

// Configuration (loaded from config files, with defaults)
var (
	wakeInterval            = 15 * time.Minute // How often to wake and process LEDs
	scheduleRefreshInterval = 3 * time.Hour    // How often to fetch schedule from MQTT
)

// Global WiFi stack reference for shutdown
var globalCyStack *cywnet.Stack

const pollTime = 5 * time.Millisecond

var requestedIP = [4]byte{192, 168, 1, 99}

// Channel for manual refresh requests from console
var refreshChan = make(chan struct{}, 1)

// Debug sleep override duration (0 = use default wakeInterval)
var debugSleepDuration time.Duration

// Functional watchdog state
var (
	lastSuccessfulRefresh time.Time
	consecutiveFailures   int
	systemHealthy         = true // When false, stop feeding watchdog to trigger reset
)

// Schedule refresh tracking (separate from watchdog state)
var lastScheduleFetch time.Time

// ForceScheduleRefresh forces the next wake cycle to refresh the schedule
// (used by manual refresh command)
var forceScheduleRefresh bool

// NTP tracking
var (
	lastNTPSync   time.Time
	ntpSyncCount  int
	ntpFailCount  int
	ntpTimeOffset time.Duration  // Last known offset from NTP
	dnsServers    []netip.Addr   // DNS servers from DHCP (for NTP lookups)
)

// Functional watchdog thresholds
const (
	maxConsecutiveFailures = 3
	maxHoursWithoutRefresh = 12
)

// fatalError handles unrecoverable errors by waiting for watchdog reset
// with a software reset fallback. This ensures the device always recovers.
func fatalError(msg string) {
	println(msg)
	// Stop feeding watchdog (in case loopForeverStack is running)
	systemHealthy = false
	// Wait for watchdog timeout (8s timeout + margin)
	// If watchdog doesn't trigger, fall back to software reset
	for i := 0; i < 15; i++ {
		time.Sleep(time.Second)
	}
	// Watchdog didn't trigger - use software reset
	println("Watchdog timeout - forcing software reset...")
	ota.Reboot()
	// Should never reach here
	for {
		time.Sleep(time.Second)
	}
}

// WiFi quality tracking
var wifiStats struct {
	connectTime      time.Time // When WiFi connected
	lastMQTTSuccess  time.Time // Last successful MQTT operation
	lastMQTTAttempt  time.Time // Last MQTT attempt
	mqttSuccessCount int       // Total successful MQTT operations
	mqttFailCount    int       // Total failed MQTT operations
	reconnectCount   int       // Number of reconnects (future use)
}

func main() {
	// CRITICAL: Confirm OTA partition IMMEDIATELY to prevent TBYB auto-revert.
	// Must be called within 16.7s of boot. Do this before ANY delays!
	confirmResult := ota.ConfirmPartitionWithCode()

	time.Sleep(2 * time.Second) // Give time to connect to USB and monitor output.
	println("========================================")
	println("  Openenterprise Bindicator")
	println("  Version:", version.Version)
	println("  Git SHA:", version.GitSHA)
	println("  Built:  ", version.BuildDate)
	println("========================================")

	// Show which partition we booted from
	currentPart := ota.GetCurrentPartition()
	if currentPart == ota.PartitionA {
		println("OTA: booted from partition A")
		// Blink 2 times slow (A)
		for i := 0; i < 2; i++ {
			setLED(BinGreen, true)
			time.Sleep(500 * time.Millisecond)
			setLED(BinGreen, false)
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		println("OTA: booted from partition B")
		// Blink 10 times fast (B)
		for i := 0; i < 10; i++ {
			setLED(BinGreen, true)
			time.Sleep(100 * time.Millisecond)
			setLED(BinGreen, false)
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Report confirm result
	if confirmResult != 0 {
		println("OTA: partition confirm returned:", confirmResult)
	} else {
		println("OTA: partition confirmed")
	}

	// Setup application logger (debug level for our code)
	// Uses telemetry.SlogHandler to bridge logs to both console and OpenTelemetry
	logger := slog.New(telemetry.NewSlogHandler(machine.Serial, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Setup network stack logger (error+4 level to suppress all network noise)
	// The cywnet library logs "packet dropped" at ERROR level which is normal for WiFi
	netLogger := slog.New(slog.NewTextHandler(machine.Serial, &slog.HandlerOptions{
		Level: slog.Level(12), // Higher than ERROR(8) to suppress all network stack logging
	}))

	// Initialize modules
	bindicatorLogger = logger // Set logger for bindicator module
	initLEDs()
	initConsole()

	// Configure watchdog for reliability (8 second timeout)
	machine.Watchdog.Configure(machine.WatchdogConfig{
		TimeoutMillis: 8000,
	})
	machine.Watchdog.Start()
	logger.Info("init:watchdog-started")

	// Log boot info
	bootPartition := "A"
	if ota.GetCurrentPartition() == ota.PartitionB {
		bootPartition = "B"
	}
	shortSHA := version.GitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	logger.Info("init:complete",
		slog.String("version", version.Version),
		slog.String("sha", shortSHA),
		slog.String("partition", bootPartition),
	)

	// Get MQTT broker address from config
	brokerAddr, err := config.BrokerAddr()
	if err != nil {
		logger.Error("config:broker-invalid", slog.String("err", err.Error()))
		fatalError("Invalid broker address - waiting for reset...")
	}
	logger.Info("config:broker", slog.String("addr", brokerAddr.String()))

	// Load timing configuration
	wakeInterval = config.WakeInterval()
	scheduleRefreshInterval = config.ScheduleRefreshInterval()
	logger.Info("config:timing",
		slog.Duration("wake_interval", wakeInterval),
		slog.Duration("schedule_refresh_interval", scheduleRefreshInterval),
	)

	// Initialize WiFi (use quieter logger for network stack)
	devcfg := cyw43439.DefaultWifiConfig()
	devcfg.Logger = netLogger
	cystack, err := cywnet.NewConfiguredPicoWithStack(
		credentials.SSID(),
		credentials.Password(),
		devcfg,
		cywnet.StackConfig{
			Hostname:    "bindicator",
			MaxTCPPorts: 3, // MQTT + debug console + OTA
		},
	)
	if err != nil {
		logger.Error("wifi:setup-failed", slog.String("err", err.Error()))
		fatalError("WiFi setup failed - waiting for reset...")
	}

	// Store global reference for OTA shutdown
	globalCyStack = cystack

	// Register WiFi shutdown callback for OTA (like Pico SDK's cyw43_arch_deinit)
	ota.SetWiFiShutdown(func() {
		// Note: TinyGo's cyw43439 driver doesn't have a full deinit,
		// but stopping processing helps ensure clean state before reboot
		logger.Info("ota:wifi-shutdown")
		time.Sleep(100 * time.Millisecond) // Allow pending packets to drain
	})

	// Start background goroutine for network stack processing
	go loopForeverStack(cystack)

	// DHCP
	dhcpResults, err := cystack.SetupWithDHCP(cywnet.DHCPConfig{
				RequestedAddr: netip.AddrFrom4(requestedIP),
	})
	if err != nil {
		logger.Error("dhcp:failed", slog.String("err", err.Error()))
		fatalError("DHCP failed - waiting for reset...")
	}
	logger.Info("dhcp:complete", slog.String("addr", dhcpResults.AssignedAddr.String()))

	// Track WiFi connection time
	wifiStats.connectTime = time.Now()

	// Store DNS servers for NTP lookups
	dnsServers = dhcpResults.DNSServers

	// Get network stack reference
	stack := cystack.LnetoStack()

	// Sync time via NTP before telemetry init (so telemetry has correct timestamps)
	logger.Info("ntp:init", slog.String("server", config.NTPServer()))
	if _, err := syncNTP(stack, dnsServers, logger); err != nil {
		// NTP failure is non-fatal, but log it prominently
		logger.Warn("ntp:init-failed", slog.String("err", err.Error()))
		logger.Warn("ntp:time-not-synced", slog.String("fallback", "MQTT timestamp"))
	}

	// Initialize telemetry (non-fatal if collector not configured)
	collectorAddr, err := config.TelemetryCollectorAddr()
	if err != nil {
		logger.Warn("telemetry:config-invalid", slog.String("err", err.Error()))
	} else if err := telemetry.Init(stack, logger, collectorAddr); err != nil {
		logger.Warn("telemetry:init-failed", slog.String("err", err.Error()))
	}

	// Start debug console server
	go consoleServer(stack, logger, refreshChan)

	// Initialize OTA update server (starts disabled, enable via 'ota-enable' console command)
	otaServerInit(stack, logger)

	// Initialize last successful refresh to now (give grace period on boot)
	lastSuccessfulRefresh = time.Now()
	// Initialize last schedule fetch to zero so first cycle always fetches
	lastScheduleFetch = time.Time{}

	// Main loop - decoupled schedule refresh from LED processing
	// LEDs are processed every wakeInterval (default 15m) for responsive updates
	// Schedule is fetched every scheduleRefreshInterval (default 3h) to reduce network load
	for {
		feedWatchdogIfHealthy()

		// Generate trace context for this wake cycle
		telemetry.GenerateTraceID(stack)

		// Start server span for wake cycle (creates X-Ray segment, not subsegment)
		cycleSpanIdx := telemetry.StartServerSpan(stack, "wake-cycle")

		// Check if schedule refresh is needed
		timeSinceLastFetch := time.Since(lastScheduleFetch)
		needsScheduleRefresh := timeSinceLastFetch >= scheduleRefreshInterval || forceScheduleRefresh
		manualRefresh := forceScheduleRefresh
		forceScheduleRefresh = false // Reset the flag

		logger.Info("cycle:start",
			slog.Duration("since_last_fetch", timeSinceLastFetch),
			slog.Bool("needs_refresh", needsScheduleRefresh),
			slog.Bool("manual_refresh", manualRefresh),
		)

		if needsScheduleRefresh {
			// Resync NTP on schedule refresh cycles to maintain accurate time
			ntpSpanIdx := telemetry.StartSpan(stack, "ntp-sync")
			if _, err := syncNTP(stack, dnsServers, logger); err != nil {
				telemetry.EndSpan(ntpSpanIdx, false)
				logger.Warn("ntp:resync-failed", slog.String("err", err.Error()))
			} else {
				telemetry.EndSpan(ntpSpanIdx, true)
			}

			feedWatchdogIfHealthy()

			// MQTT retry with exponential backoff: 16s -> 32s -> 60s (max)
			const (
				mqttMinBackoff = 16 * time.Second
				mqttMaxBackoff = 60 * time.Second
				mqttMaxRetries = 3
			)
			var mqttSuccess bool
			mqttBackoff := mqttMinBackoff

			// Start child span for MQTT refresh (covers all retries)
			mqttSpanIdx := telemetry.StartSpan(stack, "mqtt-refresh")

			for attempt := 0; attempt <= mqttMaxRetries; attempt++ {
				// Track MQTT attempt
				wifiStats.lastMQTTAttempt = time.Now()

				if attempt > 0 {
					logger.Info("mqtt:backoff",
						slog.Int("attempt", attempt+1),
						slog.Duration("wait", mqttBackoff),
					)
					sleepWithWatchdog(mqttBackoff)
					// Exponential backoff, capped at max
					mqttBackoff = mqttBackoff * 2
					if mqttBackoff > mqttMaxBackoff {
						mqttBackoff = mqttMaxBackoff
					}
				}

				feedWatchdogIfHealthy()
				logger.Info("schedule:fetching", slog.Int("attempt", attempt+1))

				// Fetch schedule via MQTT
				jobs, err := fetchScheduleViaMQTT(stack, brokerAddr, logger)
				if err != nil {
					logger.Error("mqtt:failed",
						slog.String("err", err.Error()),
						slog.Int("attempt", attempt+1),
					)
					wifiStats.mqttFailCount++

					// If more retries available, continue; otherwise fail
					if attempt < mqttMaxRetries {
						continue
					}

					// All retries exhausted
					telemetry.EndSpan(mqttSpanIdx, false)
					consecutiveFailures++
					logger.Warn("watchdog:failure-count",
						slog.Int("consecutive", consecutiveFailures),
						slog.Int("max", maxConsecutiveFailures),
					)
					logger.Info("schedule:using-cached",
						slog.Int("cached_jobs", len(getJobs())),
					)
					checkSystemHealth(logger)
				} else {
					// Success
					telemetry.EndSpan(mqttSpanIdx, true)
					wifiStats.lastMQTTSuccess = time.Now()
					wifiStats.mqttSuccessCount++
					lastScheduleFetch = time.Now()

					// Record metrics
					telemetry.RecordCounter("mqtt.success.count", int64(wifiStats.mqttSuccessCount))
					telemetry.RecordCounter("mqtt.fail.count", int64(wifiStats.mqttFailCount))

					// Success - reset failure count and update timestamp
					consecutiveFailures = 0
					lastSuccessfulRefresh = time.Now()
					logger.Info("schedule:fetched",
						slog.Int("jobs", len(jobs)),
						slog.String("time", lastSuccessfulRefresh.Format("15:04:05")),
					)
					mqttSuccess = true
					break // Exit retry loop on success
				}
			}
			_ = mqttSuccess // Avoid unused variable warning
		}

		feedWatchdogIfHealthy()

		// Always process LEDs based on current schedule (cached or fresh)
		// This ensures LEDs respond to 12-hour thresholds within wakeInterval
		ledSpanIdx := telemetry.StartSpan(stack, "led-update")
		jobs := getJobs()
		now := time.Now()
		logger.Info("leds:processing",
			slog.Int("jobs", len(jobs)),
			slog.String("time", now.Format("15:04:05")),
		)
		updateLEDsFromSchedule(jobs, now)
		logLEDState(logger)
		telemetry.EndSpan(ledSpanIdx, true)

		// End wake cycle span
		telemetry.EndSpan(cycleSpanIdx, true)

		// Sleep until next cycle, but wake early on manual refresh request
		logger.Info("sleep:starting",
			slog.Duration("duration", wakeInterval),
			slog.Duration("until_next_refresh", scheduleRefreshInterval-time.Since(lastScheduleFetch)),
		)
		sleepWithRefreshCheck(wakeInterval, refreshChan, logger)
		logger.Info("sleep:waking")
	}
}

// sleepWithRefreshCheck sleeps for the given duration but wakes early on refresh request
func sleepWithRefreshCheck(duration time.Duration, refreshChan chan struct{}, logger *slog.Logger) {
	// Use debug override if set
	if debugSleepDuration > 0 {
		duration = debugSleepDuration
		logger.Info("sleep:using-debug-duration", slog.Duration("duration", duration))
	}

	// Use 5 second intervals to keep watchdog fed (8 second timeout)
	checkInterval := 5 * time.Second
	if duration < checkInterval {
		checkInterval = duration
	}
	elapsed := time.Duration(0)

	for elapsed < duration {
		feedWatchdogIfHealthy()
		select {
		case <-refreshChan:
			logger.Info("sleep:manual-refresh-triggered")
			forceScheduleRefresh = true // Force schedule fetch on next cycle
			return
		case <-time.After(checkInterval):
			elapsed += checkInterval
		}
	}
}

// feedWatchdogIfHealthy only feeds the watchdog if the system is healthy.
// When unhealthy, the watchdog will timeout and reset the device.
func feedWatchdogIfHealthy() {
	if systemHealthy {
		machine.Watchdog.Update()
	}
}

// checkSystemHealth evaluates if the system should be considered healthy.
// Sets systemHealthy=false if thresholds are exceeded, which will cause
// the watchdog to timeout and reset the device.
func checkSystemHealth(logger *slog.Logger) {
	// Check consecutive failures
	if consecutiveFailures >= maxConsecutiveFailures {
		logger.Error("watchdog:unhealthy",
			slog.String("reason", "max consecutive failures"),
			slog.Int("failures", consecutiveFailures),
		)
		systemHealthy = false
		return
	}

	// Check hours since last success
	hoursSinceSuccess := time.Since(lastSuccessfulRefresh).Hours()
	if hoursSinceSuccess >= maxHoursWithoutRefresh {
		logger.Error("watchdog:unhealthy",
			slog.String("reason", "max hours without refresh"),
			slog.Float64("hours", hoursSinceSuccess),
		)
		systemHealthy = false
		return
	}
}

// logLEDState logs the current LED states
func logLEDState(logger *slog.Logger) {
	logger.Info("leds:state",
		slog.Bool("green", ledState.green),
		slog.Bool("black", ledState.black),
		slog.Bool("brown", ledState.brown),
	)
}

// loopForeverStack processes network packets in the background
func loopForeverStack(stack *cywnet.Stack) {
	var count int
	for {
		send, recv, _ := stack.RecvAndSend()
		if send == 0 && recv == 0 {
			time.Sleep(pollTime)
		}
		// Update watchdog every ~100 iterations (~500ms)
		count++
		if count >= 100 {
			feedWatchdogIfHealthy()
			count = 0
		}
	}
}

// NTP fallback servers if primary fails
var ntpFallbackServers = []string{
	"time.cloudflare.com",
	"time.google.com",
	"pool.ntp.org",
}

// syncNTP performs NTP time synchronization.
// Tries configured server first, then fallbacks. Tries all resolved IPs.
// Uses exponential backoff between attempts (max 30s) to avoid hammering servers.
// Returns the time offset applied, or an error if all attempts fail.
func syncNTP(stack *xnet.StackAsync, dnsServers []netip.Addr, logger *slog.Logger) (time.Duration, error) {
	// Build list of servers to try: configured first, then fallbacks
	servers := []string{config.NTPServer()}
	for _, fallback := range ntpFallbackServers {
		if fallback != servers[0] { // Don't duplicate if configured matches fallback
			servers = append(servers, fallback)
		}
	}

	rstack := stack.StackRetrying(pollTime)
	var lastErr error
	backoff := 500 * time.Millisecond // Initial backoff
	const maxBackoff = 30 * time.Second

	for _, ntpHost := range servers {
		logger.Info("ntp:trying", slog.String("server", ntpHost))
		feedWatchdogIfHealthy()

		// Small delay to let network stack settle
		time.Sleep(100 * time.Millisecond)

		// DNS lookup for NTP server
		addrs, err := rstack.DoLookupIP(ntpHost, 5*time.Second, 2)
		if err != nil {
			logger.Warn("ntp:dns-failed", slog.String("server", ntpHost), slog.String("err", err.Error()))
			lastErr = err

			// Exponential backoff before trying next server
			logger.Info("ntp:backoff", slog.Duration("wait", backoff))
			sleepWithWatchdog(backoff)
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		logger.Info("ntp:dns-resolved", slog.String("server", ntpHost), slog.Int("addrs", len(addrs)))

		// Try each resolved address
		for i, addr := range addrs {
			feedWatchdogIfHealthy()

			// Delay between attempts to let network stack process
			time.Sleep(200 * time.Millisecond)

			logger.Info("ntp:requesting", slog.String("addr", addr.String()), slog.Int("attempt", i+1))

			// Use shorter timeout per address since we'll try multiple
			offset, err := rstack.DoNTP(addr, 5*time.Second, 3)
			if err != nil {
				logger.Warn("ntp:addr-failed", slog.String("addr", addr.String()), slog.String("err", err.Error()))
				lastErr = err

				// Exponential backoff before trying next address
				logger.Info("ntp:backoff", slog.Duration("wait", backoff))
				sleepWithWatchdog(backoff)
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			// Success - apply time offset
			runtime.AdjustTimeOffset(int64(offset))
			ntpTimeOffset = offset
			lastNTPSync = time.Now()
			ntpSyncCount++

			logger.Info("ntp:synced",
				slog.String("server", ntpHost),
				slog.String("addr", addr.String()),
				slog.String("time", time.Now().Format("2006-01-02 15:04:05")),
				slog.Duration("offset", offset),
			)
			return offset, nil
		}
	}

	// All servers/addresses failed
	ntpFailCount++
	logger.Error("ntp:all-failed", slog.Int("servers_tried", len(servers)))
	return 0, lastErr
}

// sleepWithWatchdog sleeps for the given duration while keeping the watchdog fed
func sleepWithWatchdog(d time.Duration) {
	// Sleep in 2-second chunks to keep watchdog fed (8s timeout)
	for d > 0 {
		chunk := 2 * time.Second
		if d < chunk {
			chunk = d
		}
		time.Sleep(chunk)
		feedWatchdogIfHealthy()
		d -= chunk
	}
}
