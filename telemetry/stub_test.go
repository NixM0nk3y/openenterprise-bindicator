//go:build !tinygo

package telemetry

import (
	"sync"
	"time"
)

// Re-export constants for tests (these are defined in telemetry.go for tinygo)
const (
	FlushInterval = 30 * time.Second
	HTTPTimeout   = 10 * time.Second
	MaxRetries    = 2
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

// Span kind (OTLP standard)
const (
	SpanKindInternal = 1
	SpanKindServer   = 2
	SpanKindClient   = 3
)

// Pre-allocated body buffer for JSON building (test version)
var BodyBuf [2048]byte

// LogEntry represents a single log record
type LogEntry struct {
	Timestamp int64
	Severity  uint8
	BodyLen   uint8
	Body      [64]byte
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
	Kind       uint8 // SpanKindInternal, SpanKindServer, SpanKindClient
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
	mu          sync.Mutex
	enabled     bool
	HasTraceCtx bool

	// Current trace context
	CurrentTraceID [16]byte
	CurrentSpanID  [8]byte

	// Stats
	SentLogs    int
	SentMetrics int
	SentSpans   int
	SendErrors  int
)

// ResetState resets all telemetry state for testing
func ResetState() {
	mu.Lock()
	defer mu.Unlock()

	LogHead = 0
	LogCount = 0
	MetricHead = 0
	MetricCount = 0
	SpanHead = 0
	SpanCount = 0

	enabled = true
	HasTraceCtx = false

	SentLogs = 0
	SentMetrics = 0
	SentSpans = 0
	SendErrors = 0

	// Clear queues
	for i := range LogQueue {
		LogQueue[i] = LogEntry{}
	}
	for i := range MetricQueue {
		MetricQueue[i] = MetricPoint{}
	}
	for i := range SpanQueue {
		SpanQueue[i] = Span{}
	}
}

// Log queues a log entry with the given severity and message
func Log(severity uint8, msg string) {
	mu.Lock()
	defer mu.Unlock()

	if !enabled {
		return
	}

	idx := (LogHead + LogCount) % len(LogQueue)
	if LogCount >= len(LogQueue) {
		LogHead = (LogHead + 1) % len(LogQueue)
	} else {
		LogCount++
	}

	entry := &LogQueue[idx]
	entry.Timestamp = time.Now().UnixNano()
	entry.Severity = severity

	msgLen := len(msg)
	if msgLen > len(entry.Body) {
		msgLen = len(entry.Body)
	}
	entry.BodyLen = uint8(msgLen)
	copy(entry.Body[:], msg[:msgLen])

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

	if !enabled {
		return
	}

	idx := (MetricHead + MetricCount) % len(MetricQueue)
	if MetricCount >= len(MetricQueue) {
		MetricHead = (MetricHead + 1) % len(MetricQueue)
	} else {
		MetricCount++
	}

	point := &MetricQueue[idx]
	point.Timestamp = time.Now().UnixNano()
	point.Value = value
	point.IsGauge = isGauge

	nameLen := len(name)
	if nameLen > len(point.Name) {
		nameLen = len(point.Name)
	}
	point.NameLen = uint8(nameLen)
	copy(point.Name[:], name[:nameLen])
}

// SetTraceContext sets the current trace context for testing
func SetTraceContext(traceID [16]byte, spanID [8]byte) {
	mu.Lock()
	defer mu.Unlock()
	CurrentTraceID = traceID
	CurrentSpanID = spanID
	HasTraceCtx = true
}

// StartSpanTest starts a new trace span for testing (without stack dependency)
func StartSpanTest(name string) int {
	mu.Lock()
	defer mu.Unlock()

	if !enabled {
		return -1
	}

	idx := -1
	for i := 0; i < len(SpanQueue); i++ {
		if !SpanQueue[i].Active {
			idx = i
			break
		}
	}
	if idx == -1 {
		idx = SpanHead
		SpanHead = (SpanHead + 1) % len(SpanQueue)
	}

	span := &SpanQueue[idx]
	span.Active = true
	span.StartTime = time.Now().UnixNano()
	span.EndTime = 0
	span.StatusOK = false
	span.Kind = SpanKindInternal

	copy(span.TraceID[:], CurrentTraceID[:])
	copy(span.ParentID[:], CurrentSpanID[:])
	copy(span.PrevSpanID[:], CurrentSpanID[:])

	// Generate simple span ID for testing
	span.SpanID[0] = byte(idx + 1)

	copy(CurrentSpanID[:], span.SpanID[:])

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

	if SpanCount < len(SpanQueue) {
		SpanCount++
	}
}

// GetLogQueue returns the current log queue for testing
func GetLogQueue() []LogEntry {
	mu.Lock()
	defer mu.Unlock()

	result := make([]LogEntry, LogCount)
	for i := 0; i < LogCount; i++ {
		idx := (LogHead + i) % len(LogQueue)
		result[i] = LogQueue[idx]
	}
	return result
}

// GetMetricQueue returns the current metric queue for testing
func GetMetricQueue() []MetricPoint {
	mu.Lock()
	defer mu.Unlock()

	result := make([]MetricPoint, MetricCount)
	for i := 0; i < MetricCount; i++ {
		idx := (MetricHead + i) % len(MetricQueue)
		result[i] = MetricQueue[idx]
	}
	return result
}

// GetSpanQueue returns completed spans for testing
func GetSpanQueue() []Span {
	mu.Lock()
	defer mu.Unlock()

	var result []Span
	for i := 0; i < len(SpanQueue); i++ {
		if !SpanQueue[i].Active && SpanQueue[i].EndTime > 0 {
			result = append(result, SpanQueue[i])
		}
	}
	return result
}

// Enable enables telemetry
func Enable() {
	mu.Lock()
	enabled = true
	mu.Unlock()
}

// Disable disables telemetry
func Disable() {
	mu.Lock()
	enabled = false
	mu.Unlock()
}
