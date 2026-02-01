//go:build !tinygo

package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonWriter test stub for non-tinygo builds
type jsonWriter struct {
	pos int
}

func (w *jsonWriter) reset() {
	w.pos = 0
}

func (w *jsonWriter) len() int {
	return w.pos
}

func (w *jsonWriter) writeRaw(s string) {
	if w.pos+len(s) > len(BodyBuf) {
		return
	}
	copy(BodyBuf[w.pos:], s)
	w.pos += len(s)
}

func (w *jsonWriter) writeByte(b byte) {
	if w.pos < len(BodyBuf) {
		BodyBuf[w.pos] = b
		w.pos++
	}
}

func (w *jsonWriter) writeString(s string) {
	w.writeByte('"')
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch b {
		case '"':
			w.writeRaw("\\\"")
		case '\\':
			w.writeRaw("\\\\")
		case '\n':
			w.writeRaw("\\n")
		case '\r':
			w.writeRaw("\\r")
		case '\t':
			w.writeRaw("\\t")
		default:
			if b >= 32 && b < 127 {
				w.writeByte(b)
			}
		}
	}
	w.writeByte('"')
}

func (w *jsonWriter) writeBytes(b []byte, n int) {
	w.writeByte('"')
	for i := 0; i < n && i < len(b); i++ {
		c := b[i]
		switch c {
		case '"':
			w.writeRaw("\\\"")
		case '\\':
			w.writeRaw("\\\\")
		case '\n':
			w.writeRaw("\\n")
		case '\r':
			w.writeRaw("\\r")
		case '\t':
			w.writeRaw("\\t")
		default:
			if c >= 32 && c < 127 {
				w.writeByte(c)
			}
		}
	}
	w.writeByte('"')
}

func (w *jsonWriter) writeInt64(n int64) {
	w.writeByte('"')
	if n == 0 {
		w.writeByte('0')
	} else if n < 0 {
		w.writeByte('-')
		n = -n
		w.writeUint64(uint64(n))
	} else {
		w.writeUint64(uint64(n))
	}
	w.writeByte('"')
}

func (w *jsonWriter) writeUint64(n uint64) {
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	for j := i; j < len(buf); j++ {
		w.writeByte(buf[j])
	}
}

func (w *jsonWriter) writeInt(n int) {
	if n == 0 {
		w.writeByte('0')
		return
	}
	if n < 0 {
		w.writeByte('-')
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	for j := i; j < len(buf); j++ {
		w.writeByte(buf[j])
	}
}

func (w *jsonWriter) writeHex(b []byte) {
	const hexDigits = "0123456789abcdef"
	w.writeByte('"')
	for _, v := range b {
		w.writeByte(hexDigits[v>>4])
		w.writeByte(hexDigits[v&0xf])
	}
	w.writeByte('"')
}

func TestJsonWriterBasics(t *testing.T) {
	var w jsonWriter
	w.reset()

	w.writeRaw(`{"test":`)
	w.writeString("hello")
	w.writeRaw(`}`)

	result := string(BodyBuf[:w.len()])
	expected := `{"test":"hello"}`
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJsonWriterStringEscaping(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello`, `"hello"`},
		{`he"llo`, `"he\"llo"`},
		{"line1\nline2", `"line1\nline2"`},
		{`back\\slash`, `"back\\\\slash"`},
		{"tab\there", `"tab\there"`},
	}

	for _, tc := range tests {
		var w jsonWriter
		w.reset()
		w.writeString(tc.input)

		result := string(BodyBuf[:w.len()])
		if result != tc.expected {
			t.Errorf("writeString(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestJsonWriterInt64(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, `"0"`},
		{1, `"1"`},
		{-1, `"-1"`},
		{12345, `"12345"`},
		{-12345, `"-12345"`},
		{1234567890123, `"1234567890123"`},
	}

	for _, tc := range tests {
		var w jsonWriter
		w.reset()
		w.writeInt64(tc.input)

		result := string(BodyBuf[:w.len()])
		if result != tc.expected {
			t.Errorf("writeInt64(%d) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestJsonWriterInt(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, `0`},
		{1, `1`},
		{-1, `-1`},
		{12345, `12345`},
		{-999, `-999`},
	}

	for _, tc := range tests {
		var w jsonWriter
		w.reset()
		w.writeInt(tc.input)

		result := string(BodyBuf[:w.len()])
		if result != tc.expected {
			t.Errorf("writeInt(%d) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestJsonWriterHex(t *testing.T) {
	tests := []struct {
		input    []byte
		expected string
	}{
		{[]byte{0x00}, `"00"`},
		{[]byte{0xff}, `"ff"`},
		{[]byte{0x01, 0x23, 0x45, 0x67}, `"01234567"`},
		{[]byte{0xab, 0xcd, 0xef}, `"abcdef"`},
	}

	for _, tc := range tests {
		var w jsonWriter
		w.reset()
		w.writeHex(tc.input)

		result := string(BodyBuf[:w.len()])
		if result != tc.expected {
			t.Errorf("writeHex(%x) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestJsonWriterBytes(t *testing.T) {
	var w jsonWriter
	w.reset()

	data := []byte("hello world")
	w.writeBytes(data, 5) // Only write first 5 bytes

	result := string(BodyBuf[:w.len()])
	expected := `"hello"`
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestBuildLogsJSON(t *testing.T) {
	ResetState()

	// Add a log entry
	LogInfo("test:message")

	// Build JSON
	bodyLen := buildLogsJSONTest()
	if bodyLen == 0 {
		t.Fatal("buildLogsJSON returned 0")
	}

	jsonStr := string(BodyBuf[:bodyLen])

	// Verify it's valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		t.Fatalf("invalid JSON: %v\nJSON: %s", err, jsonStr)
	}

	// Verify structure
	if _, ok := data["resourceLogs"]; !ok {
		t.Error("missing resourceLogs key")
	}

	// Verify message is in the JSON
	if !strings.Contains(jsonStr, "test:message") {
		t.Error("JSON does not contain expected message")
	}

	// Verify severity is present
	if !strings.Contains(jsonStr, `"severityNumber":9`) {
		t.Error("JSON does not contain expected severity (9 for INFO)")
	}
}

func TestBuildMetricsJSON(t *testing.T) {
	ResetState()

	// Add a metric
	RecordGauge("test.gauge", 42)

	// Build JSON
	bodyLen := buildMetricsJSONTest()
	if bodyLen == 0 {
		t.Fatal("buildMetricsJSON returned 0")
	}

	jsonStr := string(BodyBuf[:bodyLen])

	// Verify it's valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		t.Fatalf("invalid JSON: %v\nJSON: %s", err, jsonStr)
	}

	// Verify structure
	if _, ok := data["resourceMetrics"]; !ok {
		t.Error("missing resourceMetrics key")
	}

	// Verify metric name is in JSON
	if !strings.Contains(jsonStr, "test.gauge") {
		t.Error("JSON does not contain expected metric name")
	}

	// Verify gauge structure
	if !strings.Contains(jsonStr, `"gauge"`) {
		t.Error("JSON does not contain gauge structure")
	}
}

func TestBuildMetricsJSONCounter(t *testing.T) {
	ResetState()

	// Add a counter
	RecordCounter("test.counter", 100)

	bodyLen := buildMetricsJSONTest()
	if bodyLen == 0 {
		t.Fatal("buildMetricsJSON returned 0")
	}

	jsonStr := string(BodyBuf[:bodyLen])

	// Verify it's valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		t.Fatalf("invalid JSON: %v\nJSON: %s", err, jsonStr)
	}

	// Verify sum structure for counter
	if !strings.Contains(jsonStr, `"sum"`) {
		t.Error("JSON does not contain sum structure for counter")
	}

	if !strings.Contains(jsonStr, `"isMonotonic":true`) {
		t.Error("JSON does not contain isMonotonic:true")
	}
}

func TestBuildSpansJSON(t *testing.T) {
	ResetState()

	// Set trace context and create a span
	var traceID [16]byte
	for i := 0; i < 16; i++ {
		traceID[i] = byte(i + 0x10)
	}
	SetTraceContext(traceID, [8]byte{})

	idx := StartSpanTest("test-span")
	EndSpan(idx, true)

	// Build JSON
	bodyLen := buildSpansJSONTest()
	if bodyLen == 0 {
		t.Fatal("buildSpansJSON returned 0")
	}

	jsonStr := string(BodyBuf[:bodyLen])

	// Verify it's valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		t.Fatalf("invalid JSON: %v\nJSON: %s", err, jsonStr)
	}

	// Verify structure
	if _, ok := data["resourceSpans"]; !ok {
		t.Error("missing resourceSpans key")
	}

	// Verify span name
	if !strings.Contains(jsonStr, "test-span") {
		t.Error("JSON does not contain expected span name")
	}

	// Verify trace ID (hex encoded)
	if !strings.Contains(jsonStr, "10111213141516171819") {
		t.Error("JSON does not contain expected trace ID hex")
	}

	// Verify status OK
	if !strings.Contains(jsonStr, `"code":1`) {
		t.Error("JSON does not contain status OK (code 1)")
	}
}

func TestBuildLogsJSONEmpty(t *testing.T) {
	ResetState()

	// No logs added
	bodyLen := buildLogsJSONTest()
	if bodyLen != 0 {
		t.Errorf("buildLogsJSON should return 0 for empty queue, got %d", bodyLen)
	}
}

func TestBuildMetricsJSONEmpty(t *testing.T) {
	ResetState()

	// No metrics added
	bodyLen := buildMetricsJSONTest()
	if bodyLen != 0 {
		t.Errorf("buildMetricsJSON should return 0 for empty queue, got %d", bodyLen)
	}
}

func TestBuildSpansJSONEmpty(t *testing.T) {
	ResetState()

	// No spans added
	bodyLen := buildSpansJSONTest()
	if bodyLen != 0 {
		t.Errorf("buildSpansJSON should return 0 for empty queue, got %d", bodyLen)
	}
}

func TestBuildLogsJSONMultiple(t *testing.T) {
	ResetState()

	// Add multiple logs
	LogDebug("debug msg")
	LogInfo("info msg")
	LogWarn("warn msg")
	LogError("error msg")

	bodyLen := buildLogsJSONTest()
	if bodyLen == 0 {
		t.Fatal("buildLogsJSON returned 0")
	}

	jsonStr := string(BodyBuf[:bodyLen])

	// Verify it's valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Verify all messages are present
	for _, msg := range []string{"debug msg", "info msg", "warn msg", "error msg"} {
		if !strings.Contains(jsonStr, msg) {
			t.Errorf("JSON missing message: %s", msg)
		}
	}
}

// buildLogsJSONTest is a test version that mimics the real BuildLogsJSON
func buildLogsJSONTest() int {
	if LogCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	w.writeRaw(`{"resourceLogs":[{"resource":{"attributes":[`)
	w.writeRaw(`{"key":"service.name","value":{"stringValue":"bindicator"}}`)
	w.writeRaw(`]},"scopeLogs":[{"logRecords":[`)

	first := true
	for i := 0; i < LogCount; i++ {
		idx := (LogHead + i) % len(LogQueue)
		entry := &LogQueue[idx]

		if !first {
			w.writeByte(',')
		}
		first = false

		w.writeRaw(`{"timeUnixNano":`)
		w.writeInt64(entry.Timestamp)
		w.writeRaw(`,"severityNumber":`)
		w.writeInt(int(entry.Severity))
		w.writeRaw(`,"body":{"stringValue":`)
		w.writeBytes(entry.Body[:], int(entry.BodyLen))
		w.writeByte('}')

		if entry.HasTrace {
			w.writeRaw(`,"traceId":`)
			w.writeHex(entry.TraceID[:])
			w.writeRaw(`,"spanId":`)
			w.writeHex(entry.SpanID[:])
		}

		w.writeByte('}')
	}

	w.writeRaw(`]}]}]}`)

	return w.len()
}

// buildMetricsJSONTest is a test version that mimics the real BuildMetricsJSON
func buildMetricsJSONTest() int {
	if MetricCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	w.writeRaw(`{"resourceMetrics":[{"resource":{"attributes":[`)
	w.writeRaw(`{"key":"service.name","value":{"stringValue":"bindicator"}}`)
	w.writeRaw(`]},"scopeMetrics":[{"metrics":[`)

	first := true
	for i := 0; i < MetricCount; i++ {
		idx := (MetricHead + i) % len(MetricQueue)
		point := &MetricQueue[idx]

		if !first {
			w.writeByte(',')
		}
		first = false

		w.writeRaw(`{"name":`)
		w.writeBytes(point.Name[:], int(point.NameLen))

		if point.IsGauge {
			w.writeRaw(`,"gauge":{"dataPoints":[{"timeUnixNano":`)
			w.writeInt64(point.Timestamp)
			w.writeRaw(`,"asInt":`)
			w.writeInt64(point.Value)
			w.writeRaw(`}]}`)
		} else {
			w.writeRaw(`,"sum":{"dataPoints":[{"timeUnixNano":`)
			w.writeInt64(point.Timestamp)
			w.writeRaw(`,"asInt":`)
			w.writeInt64(point.Value)
			w.writeRaw(`}],"aggregationTemporality":2,"isMonotonic":true}`)
		}

		w.writeByte('}')
	}

	w.writeRaw(`]}]}]}`)

	return w.len()
}

// buildSpansJSONTest is a test version that mimics the real BuildSpansJSON
func buildSpansJSONTest() int {
	completedCount := 0
	for i := 0; i < len(SpanQueue); i++ {
		span := &SpanQueue[i]
		if !span.Active && span.EndTime > 0 {
			completedCount++
		}
	}

	if completedCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	w.writeRaw(`{"resourceSpans":[{"resource":{"attributes":[`)
	w.writeRaw(`{"key":"service.name","value":{"stringValue":"bindicator"}}`)
	w.writeRaw(`]},"scopeSpans":[{"spans":[`)

	first := true
	for i := 0; i < len(SpanQueue); i++ {
		span := &SpanQueue[i]
		if span.Active || span.EndTime == 0 {
			continue
		}

		if !first {
			w.writeByte(',')
		}
		first = false

		w.writeRaw(`{"traceId":`)
		w.writeHex(span.TraceID[:])
		w.writeRaw(`,"spanId":`)
		w.writeHex(span.SpanID[:])

		hasParent := false
		for _, b := range span.ParentID {
			if b != 0 {
				hasParent = true
				break
			}
		}
		if hasParent {
			w.writeRaw(`,"parentSpanId":`)
			w.writeHex(span.ParentID[:])
		}

		w.writeRaw(`,"name":`)
		w.writeBytes(span.Name[:], int(span.NameLen))
		w.writeRaw(`,"startTimeUnixNano":`)
		w.writeInt64(span.StartTime)
		w.writeRaw(`,"endTimeUnixNano":`)
		w.writeInt64(span.EndTime)
		w.writeRaw(`,"status":{"code":`)
		if span.StatusOK {
			w.writeInt(SpanStatusOK)
		} else {
			w.writeInt(SpanStatusError)
		}
		w.writeRaw(`}}`)

		span.EndTime = 0
	}

	w.writeRaw(`]}]}]}`)

	return w.len()
}
