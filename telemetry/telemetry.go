//go:build tinygo

// Package telemetry provides OpenTelemetry-compatible logging, metrics, and tracing
// for TinyGo applications with zero-heap design.
package telemetry

import (
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/soypat/lneto/tcp"
	"github.com/soypat/lneto/x/xnet"
)

// Configuration constants
const (
	FlushInterval  = 30 * time.Second
	HTTPTimeout    = 10 * time.Second
	MaxRetries     = 2
)

// Log severity levels (OTLP standard)
const (
	SeverityDebug = 5
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17
)

// Span status codes (OTLP standard)
const (
	SpanStatusUnset = 0
	SpanStatusOK    = 1
	SpanStatusError = 2
)

// Pre-allocated TCP buffers (~3KB)
// TxBuf must be large enough for body (2KB) + headers (~200 bytes)
var (
	tcpRxBuf [512]byte
	tcpTxBuf [2560]byte
)

// Pre-allocated body and response buffers (~2.3KB)
var (
	BodyBuf [2048]byte
	respBuf [256]byte
)

// LogEntry represents a single log record
type LogEntry struct {
	Timestamp int64
	Severity  uint8
	BodyLen   uint8
	Body      [128]byte
	TraceID   [16]byte
	SpanID    [8]byte
	HasTrace  bool
}

// MetricPoint represents a single metric data point
type MetricPoint struct {
	Timestamp int64
	Value     int64
	NameLen   uint8
	Name      [32]byte
	IsGauge   bool
}

// Span represents a trace span
type Span struct {
	TraceID    [16]byte
	SpanID     [8]byte
	ParentID   [8]byte
	PrevSpanID [8]byte // Previous CurrentSpanID to restore on EndSpan
	StartTime  int64
	EndTime    int64
	NameLen    uint8
	Name       [32]byte
	StatusOK   bool
	Active     bool
}

// Circular queues for telemetry data
var (
	LogQueue    [8]LogEntry
	LogHead     int
	LogCount    int
	MetricQueue [8]MetricPoint
	MetricHead  int
	MetricCount int
	SpanQueue   [4]Span
	SpanHead    int
	SpanCount   int
)

// Telemetry state
var (
	mu        sync.Mutex
	enabled   bool
	paused    bool // Paused during OTA or other critical operations
	sendingWg sync.WaitGroup // Tracks in-progress HTTP operations
	stack     *xnet.StackAsync
	logger    *slog.Logger
	collector netip.AddrPort

	// Current trace context (set by GenerateTraceID)
	CurrentTraceID [16]byte
	CurrentSpanID  [8]byte
	HasTraceCtx    bool

	// Stats
	SentLogs    int
	SentMetrics int
	SentSpans   int
	SendErrors  int
)

// Init initializes the telemetry module with the given network stack and collector address.
func Init(s *xnet.StackAsync, log *slog.Logger, collectorAddr netip.AddrPort) error {
	mu.Lock()
	stack = s
	logger = log
	collector = collectorAddr
	enabled = true
	mu.Unlock()

	// Start background sender goroutine
	go senderLoop()

	if log != nil {
		log.Info("telemetry:init", slog.String("collector", collectorAddr.String()))
	}

	return nil
}

// Log queues a log entry with the given severity and message
func Log(severity uint8, msg string) {
	mu.Lock()
	defer mu.Unlock()

	if !enabled || paused {
		return
	}

	// Find slot in circular queue
	idx := (LogHead + LogCount) % len(LogQueue)
	if LogCount >= len(LogQueue) {
		// Queue full, overwrite oldest
		LogHead = (LogHead + 1) % len(LogQueue)
	} else {
		LogCount++
	}

	entry := &LogQueue[idx]
	entry.Timestamp = time.Now().UnixNano()
	entry.Severity = severity

	// Copy message (truncate if needed)
	msgLen := len(msg)
	if msgLen > len(entry.Body) {
		msgLen = len(entry.Body)
	}
	entry.BodyLen = uint8(msgLen)
	copy(entry.Body[:], msg[:msgLen])

	// Copy current trace context if available
	entry.HasTrace = HasTraceCtx
	if HasTraceCtx {
		copy(entry.TraceID[:], CurrentTraceID[:])
		copy(entry.SpanID[:], CurrentSpanID[:])
	}
}

// LogDebug logs a debug message
func LogDebug(msg string) {
	Log(SeverityDebug, msg)
}

// LogInfo logs an info message
func LogInfo(msg string) {
	Log(SeverityInfo, msg)
}

// LogWarn logs a warning message
func LogWarn(msg string) {
	Log(SeverityWarn, msg)
}

// LogError logs an error message
func LogError(msg string) {
	Log(SeverityError, msg)
}

// RecordGauge records a point-in-time gauge metric
func RecordGauge(name string, value int64) {
	recordMetric(name, value, true)
}

// RecordCounter records a monotonic counter metric
func RecordCounter(name string, value int64) {
	recordMetric(name, value, false)
}

func recordMetric(name string, value int64, isGauge bool) {
	mu.Lock()
	defer mu.Unlock()

	if !enabled || paused {
		return
	}

	// Find slot in circular queue
	idx := (MetricHead + MetricCount) % len(MetricQueue)
	if MetricCount >= len(MetricQueue) {
		// Queue full, overwrite oldest
		MetricHead = (MetricHead + 1) % len(MetricQueue)
	} else {
		MetricCount++
	}

	point := &MetricQueue[idx]
	point.Timestamp = time.Now().UnixNano()
	point.Value = value
	point.IsGauge = isGauge

	// Copy name (truncate if needed)
	nameLen := len(name)
	if nameLen > len(point.Name) {
		nameLen = len(point.Name)
	}
	point.NameLen = uint8(nameLen)
	copy(point.Name[:], name[:nameLen])
}

// GenerateTraceID generates a new trace ID using the stack's PRNG.
// The trace ID format is X-Ray compatible:
// - First 4 bytes: Unix timestamp in seconds (big-endian)
// - Remaining 12 bytes: Random
func GenerateTraceID(s *xnet.StackAsync) {
	mu.Lock()
	defer mu.Unlock()

	// X-Ray compatible trace ID: first 4 bytes are Unix timestamp (seconds)
	ts := uint32(time.Now().Unix())
	CurrentTraceID[0] = byte(ts >> 24)
	CurrentTraceID[1] = byte(ts >> 16)
	CurrentTraceID[2] = byte(ts >> 8)
	CurrentTraceID[3] = byte(ts)

	// Remaining 12 bytes are random (3 uint32s)
	for i := 0; i < 3; i++ {
		r := s.Prand32()
		CurrentTraceID[4+i*4] = byte(r >> 24)
		CurrentTraceID[4+i*4+1] = byte(r >> 16)
		CurrentTraceID[4+i*4+2] = byte(r >> 8)
		CurrentTraceID[4+i*4+3] = byte(r)
	}

	// Generate 8 bytes for span ID (2 uint32s)
	for i := 0; i < 2; i++ {
		r := s.Prand32()
		CurrentSpanID[i*4] = byte(r >> 24)
		CurrentSpanID[i*4+1] = byte(r >> 16)
		CurrentSpanID[i*4+2] = byte(r >> 8)
		CurrentSpanID[i*4+3] = byte(r)
	}

	HasTraceCtx = true
}

// StartSpan starts a new trace span and returns its index
func StartSpan(s *xnet.StackAsync, name string) int {
	mu.Lock()
	defer mu.Unlock()

	if !enabled || paused {
		return -1
	}

	// Find an inactive slot or use circular queue
	idx := -1
	for i := 0; i < len(SpanQueue); i++ {
		if !SpanQueue[i].Active {
			idx = i
			break
		}
	}
	if idx == -1 {
		// All slots active, use oldest
		idx = SpanHead
		SpanHead = (SpanHead + 1) % len(SpanQueue)
	}

	span := &SpanQueue[idx]
	span.Active = true
	span.StartTime = time.Now().UnixNano()
	span.EndTime = 0
	span.StatusOK = false

	// Copy trace context and save previous span ID for restoration on EndSpan
	copy(span.TraceID[:], CurrentTraceID[:])
	copy(span.ParentID[:], CurrentSpanID[:])
	copy(span.PrevSpanID[:], CurrentSpanID[:])

	// Generate new span ID
	r1 := s.Prand32()
	r2 := s.Prand32()
	span.SpanID[0] = byte(r1 >> 24)
	span.SpanID[1] = byte(r1 >> 16)
	span.SpanID[2] = byte(r1 >> 8)
	span.SpanID[3] = byte(r1)
	span.SpanID[4] = byte(r2 >> 24)
	span.SpanID[5] = byte(r2 >> 16)
	span.SpanID[6] = byte(r2 >> 8)
	span.SpanID[7] = byte(r2)

	// Update current span ID for child spans/logs
	copy(CurrentSpanID[:], span.SpanID[:])

	// Copy name (truncate if needed)
	nameLen := len(name)
	if nameLen > len(span.Name) {
		nameLen = len(span.Name)
	}
	span.NameLen = uint8(nameLen)
	copy(span.Name[:], name[:nameLen])

	return idx
}

// EndSpan completes a span with the given status
func EndSpan(idx int, statusOK bool) {
	mu.Lock()
	defer mu.Unlock()

	if idx < 0 || idx >= len(SpanQueue) {
		return
	}

	span := &SpanQueue[idx]
	if !span.Active {
		return
	}

	span.EndTime = time.Now().UnixNano()
	span.StatusOK = statusOK
	span.Active = false

	// Restore previous span ID so sibling spans have correct parent
	copy(CurrentSpanID[:], span.PrevSpanID[:])

	// Add to completed span count
	if SpanCount < len(SpanQueue) {
		SpanCount++
	}
}

// senderLoop runs in the background and flushes queues periodically
func senderLoop() {
	for {
		time.Sleep(FlushInterval)

		mu.Lock()
		isEnabled := enabled
		isPaused := paused
		mu.Unlock()

		if !isEnabled || isPaused {
			continue
		}

		// Send all telemetry types each cycle
		flushLogs()
		flushMetrics()
		flushSpans()
	}
}

// Pause temporarily stops telemetry sending (for OTA or other critical operations).
// Blocks until any in-progress HTTP operations complete to avoid network contention.
func Pause() {
	mu.Lock()
	paused = true
	mu.Unlock()

	// Wait for any in-progress HTTP operations to complete
	sendingWg.Wait()
}

// Resume resumes telemetry sending after a pause
func Resume() {
	mu.Lock()
	paused = false
	mu.Unlock()
}

// IsPaused returns true if telemetry is paused
func IsPaused() bool {
	mu.Lock()
	defer mu.Unlock()
	return paused
}

// Flush triggers an immediate flush of all queues
func Flush() {
	flushLogs()
	flushMetrics()
	flushSpans()
}

// flushLogs sends queued log entries to the collector
func flushLogs() {
	mu.Lock()
	if LogCount == 0 || !enabled || paused {
		mu.Unlock()
		return
	}

	// Build JSON payload
	bodyLen := BuildLogsJSON()
	count := LogCount

	// Clear queue
	LogHead = 0
	LogCount = 0
	mu.Unlock()

	if bodyLen == 0 {
		return
	}

	// Send HTTP POST
	err := sendHTTPPost("/v1/logs", bodyLen)
	if err != nil {
		mu.Lock()
		SendErrors++
		mu.Unlock()
		if logger != nil {
			logger.Debug("telemetry:logs-failed", slog.String("err", err.Error()))
		}
		return
	}

	mu.Lock()
	SentLogs += count
	mu.Unlock()
}

// flushMetrics sends queued metric points to the collector
func flushMetrics() {
	mu.Lock()
	if MetricCount == 0 || !enabled || paused {
		mu.Unlock()
		return
	}

	// Build JSON payload
	bodyLen := BuildMetricsJSON()
	count := MetricCount

	// Clear queue
	MetricHead = 0
	MetricCount = 0
	mu.Unlock()

	if bodyLen == 0 {
		return
	}

	// Send HTTP POST
	err := sendHTTPPost("/v1/metrics", bodyLen)
	if err != nil {
		mu.Lock()
		SendErrors++
		mu.Unlock()
		if logger != nil {
			logger.Debug("telemetry:metrics-failed", slog.String("err", err.Error()))
		}
		return
	}

	mu.Lock()
	SentMetrics += count
	mu.Unlock()
}

// flushSpans sends completed spans to the collector
func flushSpans() {
	mu.Lock()
	if SpanCount == 0 || !enabled || paused {
		mu.Unlock()
		return
	}

	// Build JSON payload
	bodyLen := BuildSpansJSON()
	count := SpanCount

	// Clear completed spans
	SpanCount = 0
	mu.Unlock()

	if bodyLen == 0 {
		return
	}

	// Send HTTP POST
	err := sendHTTPPost("/v1/traces", bodyLen)
	if err != nil {
		mu.Lock()
		SendErrors++
		mu.Unlock()
		if logger != nil {
			logger.Debug("telemetry:spans-failed", slog.String("err", err.Error()))
		}
		return
	}

	mu.Lock()
	SentSpans += count
	mu.Unlock()
}

// sendHTTPPost sends an HTTP POST request to the collector
func sendHTTPPost(path string, bodyLen int) error {
	// Track this operation so Pause() can wait for it to complete
	sendingWg.Add(1)
	defer sendingWg.Done()

	mu.Lock()
	s := stack
	c := collector
	mu.Unlock()

	if s == nil {
		return errors.New("no stack")
	}

	// Configure TCP connection (match MQTT settings)
	var conn tcp.Conn
	err := conn.Configure(tcp.ConnConfig{
		RxBuf:             tcpRxBuf[:],
		TxBuf:             tcpTxBuf[:],
		TxPacketQueueSize: 3,
	})
	if err != nil {
		return err
	}

	// Create retrying stack for dial
	rstack := s.StackRetrying(5 * time.Millisecond)

	// Random local port
	lport := uint16(s.Prand32()>>17) + 1024

	// Dial with timeout and retries
	err = rstack.DoDialTCP(&conn, lport, c, HTTPTimeout, MaxRetries)
	if err != nil {
		conn.Abort()
		return err
	}

	// Give the stack time to fully establish connection
	time.Sleep(50 * time.Millisecond)

	// Verify connection is ready
	if !conn.State().IsSynchronized() {
		conn.Abort()
		return errors.New("connection not established")
	}

	// Build and send HTTP request
	conn.SetDeadline(time.Now().Add(HTTPTimeout))

	// Write HTTP headers
	conn.Write([]byte("POST "))
	conn.Write([]byte(path))
	conn.Write([]byte(" HTTP/1.1\r\nHost: "))
	conn.Write([]byte(c.Addr().String()))
	conn.Write([]byte("\r\nContent-Type: application/json\r\nContent-Length: "))
	writeHTTPInt(&conn, bodyLen)
	conn.Write([]byte("\r\nConnection: close\r\n\r\n"))

	// Flush headers and give stack time to process
	conn.Flush()
	time.Sleep(50 * time.Millisecond)

	// Write body in chunks if large (tx buffer may not hold all)
	written := 0
	for written < bodyLen {
		chunk := bodyLen - written
		if chunk > 1024 {
			chunk = 1024
		}
		n, err := conn.Write(BodyBuf[written : written+chunk])
		if err != nil {
			conn.Abort()
			return errors.New("write failed: body")
		}
		written += n
		// Flush each chunk and yield to stack
		conn.Flush()
		time.Sleep(50 * time.Millisecond)
	}

	// Final wait for transmission
	time.Sleep(50 * time.Millisecond)

	// Read response (just check for 2xx status)
	respLen, _ := conn.Read(respBuf[:])

	// Close connection gracefully
	conn.Close()
	// Wait up to 1 second for graceful close
	for i := 0; i < 10 && !conn.State().IsClosed(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	conn.Abort()

	// Discard ARP query to free slot for next connection
	s.DiscardResolveHardwareAddress6(c.Addr())

	// Check for success (HTTP/1.1 2xx)
	if respLen >= 12 {
		// Look for "HTTP/1.1 2" or "HTTP/1.0 2"
		if respBuf[9] == '2' {
			return nil
		}
	}

	return errors.New("http error")
}

// writeHTTPInt writes an integer to the TCP connection
func writeHTTPInt(conn *tcp.Conn, n int) {
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

// Status returns current telemetry statistics
func Status() (isEnabled bool, queuedLogs, queuedMetrics, queuedSpans int,
	sentLogs, sentMetrics, sentSpans, errs int, collectorAddr string) {
	mu.Lock()
	defer mu.Unlock()

	return enabled, LogCount, MetricCount, SpanCount,
		SentLogs, SentMetrics, SentSpans,
		SendErrors, collector.String()
}

// Disable disables telemetry sending
func Disable() {
	mu.Lock()
	enabled = false
	mu.Unlock()
}

// Enable enables telemetry sending
func Enable() {
	mu.Lock()
	enabled = true
	mu.Unlock()
}
