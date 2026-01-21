package config

import (
	_ "embed"
	"net/netip"
	"strings"
)

var (
	//go:embed broker.text
	brokerAddr string

	//go:embed clientid.text
	clientID string
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
