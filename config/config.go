package config

import (
	_ "embed"
	"net/netip"
	"strings"
	"time"
)

// Defaults for operational configuration.
// These can be overridden by placing a non-empty value in the corresponding .text file.
const (
	DefaultWakeInterval            = 15 * time.Minute
	DefaultScheduleRefreshInterval = 3 * time.Hour
	DefaultNTPServer               = "time.cloudflare.com"
)

// Environment-specific configuration (must be provided via embedded text files).
var (
	//go:embed broker.text
	brokerAddr string

	//go:embed clientid.text
	clientID string

	//go:embed telemetry_collector.text
	telemetryCollector string
)

// Optional overrides for defaults (empty file = use default).
var (
	//go:embed wake_interval.text
	wakeIntervalOverride string

	//go:embed schedule_refresh_interval.text
	scheduleRefreshIntervalOverride string

	//go:embed ntp_server.text
	ntpServerOverride string
)

// BrokerAddr returns the MQTT broker address from broker.text file.
// Format: "host:port" e.g., "192.168.1.100:1883"
func BrokerAddr() (netip.AddrPort, error) {
	addr := strings.TrimSpace(brokerAddr)
	return netip.ParseAddrPort(addr)
}

// ClientID returns the MQTT client ID from clientid.text file.
func ClientID() string {
	return strings.TrimSpace(clientID)
}

// TelemetryCollectorAddr returns the telemetry collector address from telemetry_collector.text file.
// Format: "host:port" e.g., "192.168.1.100:4318"
func TelemetryCollectorAddr() (netip.AddrPort, error) {
	addr := strings.TrimSpace(telemetryCollector)
	return netip.ParseAddrPort(addr)
}

// WakeInterval returns how often the device wakes to process LED states.
// Returns DefaultWakeInterval unless overridden via wake_interval.text.
func WakeInterval() time.Duration {
	if override := strings.TrimSpace(wakeIntervalOverride); override != "" {
		if d, err := time.ParseDuration(override); err == nil {
			return d
		}
	}
	return DefaultWakeInterval
}

// ScheduleRefreshInterval returns how often the device fetches a new schedule from MQTT.
// Returns DefaultScheduleRefreshInterval unless overridden via schedule_refresh_interval.text.
func ScheduleRefreshInterval() time.Duration {
	if override := strings.TrimSpace(scheduleRefreshIntervalOverride); override != "" {
		if d, err := time.ParseDuration(override); err == nil {
			return d
		}
	}
	return DefaultScheduleRefreshInterval
}

// NTPServer returns the NTP server hostname for time synchronization.
// Returns DefaultNTPServer unless overridden via ntp_server.text.
func NTPServer() string {
	if override := strings.TrimSpace(ntpServerOverride); override != "" {
		return override
	}
	return DefaultNTPServer
}
