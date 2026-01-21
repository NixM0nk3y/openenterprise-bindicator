//go:build tinygo

package main

// WARNING: default -scheduler=cores unsupported, compile with -scheduler=tasks set!

import (
	"log/slog"
	"machine"
	"net/netip"
	"time"

	"openenterprise/bindicator/config"
	"openenterprise/bindicator/credentials"
	"openenterprise/bindicator/ota"
	"openenterprise/bindicator/version"

	"github.com/soypat/cyw43439"
	"github.com/soypat/cyw43439/examples/cywnet"
)

// Configuration
var (
	wakeInterval = 3 * time.Hour
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
	logger := slog.New(slog.NewTextHandler(machine.Serial, &slog.HandlerOptions{
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

	logger.Info("init:complete")

	// Get MQTT broker address from config
	brokerAddr, err := config.BrokerAddr()
	if err != nil {
		logger.Error("config:broker-invalid", slog.String("err", err.Error()))
		fatalError("Invalid broker address - waiting for reset...")
	}
	logger.Info("config:broker", slog.String("addr", brokerAddr.String()))

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

	// Get network stack reference
	stack := cystack.LnetoStack()

	// Start debug console server
	go consoleServer(stack, logger, refreshChan)

	// Initialize OTA update server (starts disabled, enable via 'ota-enable' console command)
	otaServerInit(stack, logger)

	// Initialize last successful refresh to now (give grace period on boot)
	lastSuccessfulRefresh = time.Now()

	// Main loop - time is synced via MQTT response
	for {
		feedWatchdogIfHealthy()

		// Track MQTT attempt
		wifiStats.lastMQTTAttempt = time.Now()

		// Fetch schedule via MQTT (also syncs time from response)
		jobs, err := fetchScheduleViaMQTT(stack, brokerAddr, logger)
		if err != nil {
			logger.Error("mqtt:failed", slog.String("err", err.Error()))
			wifiStats.mqttFailCount++
			consecutiveFailures++
			logger.Warn("watchdog:failure-count",
				slog.Int("consecutive", consecutiveFailures),
				slog.Int("max", maxConsecutiveFailures),
			)
			logger.Info("leds:keeping-previous-state")
			checkSystemHealth(logger)
			goto sleep
		}

		// Track MQTT success
		wifiStats.lastMQTTSuccess = time.Now()
		wifiStats.mqttSuccessCount++

		// Success - reset failure count and update timestamp
		consecutiveFailures = 0
		lastSuccessfulRefresh = time.Now()
		logger.Info("watchdog:refresh-success",
			slog.String("time", lastSuccessfulRefresh.Format("15:04:05")),
		)

		feedWatchdogIfHealthy()

		// Update LEDs based on schedule
		logger.Info("schedule:updating-leds", slog.Int("jobs", len(jobs)))
		updateLEDsFromSchedule(jobs, time.Now())
		logLEDState(logger)

	sleep:
		// Sleep until next cycle, but wake early on manual refresh request
		logger.Info("sleep:starting", slog.Duration("duration", wakeInterval))
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
