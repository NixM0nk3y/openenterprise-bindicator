//go:build tinygo

package main

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"runtime"
	"time"

	"openenterprise/bindicator/config"

	"github.com/soypat/lneto/tcp"
	"github.com/soypat/lneto/x/xnet"
	mqtt "github.com/soypat/natiu-mqtt"
)

const (
	mqttTimeout    = 10 * time.Second
	mqttRetries    = 3
	tcpBufSize     = 2030 // MTU - ethhdr - iphdr - tcphdr
	mqttBufSize    = 512
	responseWaitMs = 5000 // Max wait for response in ms
)

// MQTT topics
var (
	topicRequest  = []byte("bindicator/request")
	topicResponse = []byte("bindicator/response")
)

// Pre-allocated buffers for memory efficiency
var (
	tcpRxBuf    [tcpBufSize]byte
	tcpTxBuf    [tcpBufSize]byte
	mqttUserBuf [mqttBufSize]byte
	responseBuf [mqttBufSize]byte
	responseLen int
	gotResponse bool

	// Subscribe request variable (reused)
	varSub = mqtt.VariablesSubscribe{
		TopicFilters: []mqtt.SubscribeRequest{
			{TopicFilter: topicResponse, QoS: mqtt.QoS0},
		},
	}
)

// MQTT publish flags (QoS0, not retained, not dup)
var pubFlags, _ = mqtt.NewPublishFlags(mqtt.QoS0, false, false)

// fetchScheduleViaMQTT connects to the MQTT broker, publishes a request,
// waits for the response, and parses it into jobs. Time is synced from response.
func fetchScheduleViaMQTT(
	stack *xnet.StackAsync,
	brokerAddr netip.AddrPort,
	logger *slog.Logger,
) ([]BinJob, error) {
	// Create retrying stack for dial with retries
	rstack := stack.StackRetrying(5 * time.Millisecond)
	// Reset response state
	gotResponse = false
	responseLen = 0

	// Configure TCP connection with pre-allocated buffers
	var conn tcp.Conn
	err := conn.Configure(tcp.ConnConfig{
		RxBuf:             tcpRxBuf[:],
		TxBuf:             tcpTxBuf[:],
		TxPacketQueueSize: 3,
	})
	if err != nil {
		return nil, err
	}

	// MQTT client configuration with zero-allocation decoder
	cfg := mqtt.ClientConfig{
		Decoder: mqtt.DecoderNoAlloc{UserBuffer: mqttUserBuf[:]},
		OnPub:   onMQTTMessage,
	}

	var varconn mqtt.VariablesConnect
	// Append random suffix to client ID to avoid conflicts with parallel units
	clientID := make([]byte, 0, 32)
	clientID = append(clientID, config.ClientID()...)
	clientID = append(clientID, '-')
	clientID = appendHex(clientID, uint16(stack.Prand32()))
	varconn.SetDefaultMQTT(clientID)
	client := mqtt.NewClient(cfg)

	// Random local port
	lport := uint16(stack.Prand32()>>17) + 1024
	logger.Info("mqtt:dialing",
		slog.String("broker", brokerAddr.String()),
		slog.String("clientid", string(clientID)),
		slog.Uint64("localport", uint64(lport)),
	)

	// Dial TCP with retries
	err = rstack.DoDialTCP(&conn, lport, brokerAddr, mqttTimeout, mqttRetries)
	if err != nil {
		logger.Error("mqtt:dial-failed", slog.String("err", err.Error()))
		closeConn(&conn, stack, brokerAddr)
		return nil, err
	}

	// Start MQTT connection
	logger.Info("mqtt:connecting")
	conn.SetDeadline(time.Now().Add(mqttTimeout))
	err = client.StartConnect(&conn, &varconn)
	if err != nil {
		logger.Error("mqtt:start-connect-failed", slog.String("err", err.Error()))
		closeConn(&conn, stack, brokerAddr)
		return nil, err
	}

	// Wait for MQTT connection
	retries := 50
	for retries > 0 && !client.IsConnected() {
		time.Sleep(100 * time.Millisecond)
		err = client.HandleNext()
		if err != nil {
			logger.Warn("mqtt:handle-next", slog.String("err", err.Error()))
		}
		retries--
	}
	if !client.IsConnected() {
		logger.Error("mqtt:connect-timeout")
		closeConn(&conn, stack, brokerAddr)
		return nil, errors.New("mqtt connect timeout")
	}
	logger.Info("mqtt:connected")

	// Subscribe to response topic using StartSubscribe (non-blocking, no context needed)
	conn.SetDeadline(time.Now().Add(mqttTimeout))
	varSub.PacketIdentifier = uint16(stack.Prand32())
	err = client.StartSubscribe(varSub)
	if err != nil {
		logger.Error("mqtt:subscribe-failed", slog.String("err", err.Error()))
		closeConn(&conn, stack, brokerAddr)
		return nil, err
	}
	logger.Info("mqtt:subscribed", slog.String("topic", string(topicResponse)))

	// Handle subscription acknowledgment
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		client.HandleNext()
	}

	// Publish request
	conn.SetDeadline(time.Now().Add(mqttTimeout))
	pubVar := mqtt.VariablesPublish{
		TopicName:        topicRequest,
		PacketIdentifier: uint16(stack.Prand32()),
	}
	err = client.PublishPayload(pubFlags, pubVar, []byte("ping"))
	if err != nil {
		logger.Error("mqtt:publish-failed", slog.String("err", err.Error()))
		closeConn(&conn, stack, brokerAddr)
		return nil, err
	}
	logger.Info("mqtt:published", slog.String("topic", string(topicRequest)))

	// Wait for response
	waitTime := 0
	for !gotResponse && waitTime < responseWaitMs {
		time.Sleep(100 * time.Millisecond)
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		client.HandleNext()
		waitTime += 100
	}

	// Disconnect cleanly
	client.Disconnect(errors.New("session complete"))
	closeConn(&conn, stack, brokerAddr)

	if !gotResponse {
		logger.Error("mqtt:no-response")
		return nil, errors.New("no response from broker")
	}

	logger.Info("mqtt:response-received", slog.Int("bytes", responseLen))

	// Parse the response
	count := parseScheduleResponse(responseBuf[:responseLen])
	logger.Info("mqtt:parsed", slog.Int("jobs", count))

	// Sync time from Node-RED timestamp
	if parsedTimestamp > 0 {
		serverTime := time.Unix(parsedTimestamp, 0)
		offset := serverTime.Sub(time.Now())
		runtime.AdjustTimeOffset(int64(offset))
		logger.Info("mqtt:time-synced",
			slog.String("time", time.Now().Format("2006-01-02 15:04:05")),
		)
	}

	return getJobs(), nil
}

// onMQTTMessage handles incoming MQTT messages
func onMQTTMessage(pubHead mqtt.Header, varPub mqtt.VariablesPublish, r io.Reader) error {
	// Check if this is the response topic
	if !bytesEqual(varPub.TopicName, topicResponse) {
		return nil
	}

	// Read the payload into response buffer
	n, err := r.Read(responseBuf[:])
	if err != nil && err != io.EOF {
		return err
	}

	responseLen = n
	gotResponse = true
	return nil
}

// closeConn closes the TCP connection and waits for it to close
func closeConn(conn *tcp.Conn, stack *xnet.StackAsync, addr netip.AddrPort) {
	conn.Close()
	for i := 0; i < 50 && !conn.State().IsClosed(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	conn.Abort()

	// Discard ARP query to free slot for next connection
	stack.DiscardResolveHardwareAddress6(addr.Addr())
}

// bytesEqual compares two byte slices without allocation
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendHex appends a uint16 as 4 hex characters to the byte slice
func appendHex(b []byte, v uint16) []byte {
	const hexDigits = "0123456789abcdef"
	return append(b,
		hexDigits[(v>>12)&0xf],
		hexDigits[(v>>8)&0xf],
		hexDigits[(v>>4)&0xf],
		hexDigits[v&0xf],
	)
}
