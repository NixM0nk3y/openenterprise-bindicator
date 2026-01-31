//go:build tinygo

package telemetry

import (
	"openenterprise/bindicator/version"
)

// jsonWriter is a zero-allocation JSON writer that writes to BodyBuf
type jsonWriter struct {
	pos int
}

// reset resets the writer position
func (w *jsonWriter) reset() {
	w.pos = 0
}

// len returns the current length
func (w *jsonWriter) len() int {
	return w.pos
}

// writeRaw writes raw bytes
func (w *jsonWriter) writeRaw(s string) {
	if w.pos+len(s) > len(BodyBuf) {
		return
	}
	copy(BodyBuf[w.pos:], s)
	w.pos += len(s)
}

// writeByte writes a single byte
func (w *jsonWriter) writeByte(b byte) {
	if w.pos < len(BodyBuf) {
		BodyBuf[w.pos] = b
		w.pos++
	}
}

// writeString writes a JSON string value (with quotes)
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
			} else {
				// Skip non-printable characters
			}
		}
	}
	w.writeByte('"')
}

// writeBytes writes a JSON string from a byte slice
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

// writeInt64 writes an int64 as a JSON string (OTLP uses string for large numbers)
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

// writeUint64 writes digits of a uint64
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

// writeInt writes an integer directly (not as string)
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

// writeHex writes a byte slice as hex string (for trace/span IDs)
func (w *jsonWriter) writeHex(b []byte) {
	const hexDigits = "0123456789abcdef"
	w.writeByte('"')
	for _, v := range b {
		w.writeByte(hexDigits[v>>4])
		w.writeByte(hexDigits[v&0xf])
	}
	w.writeByte('"')
}

// writeResourceAttributes writes common resource attributes
func (w *jsonWriter) writeResourceAttributes() {
	w.writeRaw(`"resource":{"attributes":[`)
	w.writeRaw(`{"key":"service.name","value":{"stringValue":"bindicator"}},`)
	w.writeRaw(`{"key":"service.version","value":{"stringValue":`)
	w.writeString(version.Version)
	w.writeRaw(`}},`)
	w.writeRaw(`{"key":"service.instance.id","value":{"stringValue":`)
	w.writeString(shortSHA())
	w.writeRaw(`}},`)
	w.writeRaw(`{"key":"host.name","value":{"stringValue":"bindicator-pico"}}`)
	w.writeRaw(`]}`)
}

// shortSHA returns the first 7 characters of the git SHA
func shortSHA() string {
	if len(version.GitSHA) >= 7 {
		return version.GitSHA[:7]
	}
	return version.GitSHA
}

// BuildLogsJSON builds the OTLP JSON payload for logs
// Returns the length of the payload in BodyBuf
func BuildLogsJSON() int {
	if LogCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	// Start resourceLogs
	w.writeRaw(`{"resourceLogs":[{`)
	w.writeResourceAttributes()
	w.writeRaw(`,"scopeLogs":[{"scope":{"name":"bindicator"},"logRecords":[`)

	// Write each log entry
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

		// Add trace context if available
		if entry.HasTrace {
			w.writeRaw(`,"traceId":`)
			w.writeHex(entry.TraceID[:])
			w.writeRaw(`,"spanId":`)
			w.writeHex(entry.SpanID[:])
		}

		w.writeByte('}')
	}

	// Close JSON
	w.writeRaw(`]}]}]}`)

	return w.len()
}

// BuildMetricsJSON builds the OTLP JSON payload for metrics
// Returns the length of the payload in BodyBuf
func BuildMetricsJSON() int {
	if MetricCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	// Start resourceMetrics
	w.writeRaw(`{"resourceMetrics":[{`)
	w.writeResourceAttributes()
	w.writeRaw(`,"scopeMetrics":[{"scope":{"name":"bindicator"},"metrics":[`)

	// Write each metric
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
			// Gauge metric
			w.writeRaw(`,"gauge":{"dataPoints":[{"timeUnixNano":`)
			w.writeInt64(point.Timestamp)
			w.writeRaw(`,"asInt":`)
			w.writeInt64(point.Value)
			w.writeRaw(`}]}`)
		} else {
			// Sum/Counter metric
			w.writeRaw(`,"sum":{"dataPoints":[{"timeUnixNano":`)
			w.writeInt64(point.Timestamp)
			w.writeRaw(`,"asInt":`)
			w.writeInt64(point.Value)
			w.writeRaw(`}],"aggregationTemporality":2,"isMonotonic":true}`)
		}

		w.writeByte('}')
	}

	// Close JSON
	w.writeRaw(`]}]}]}`)

	return w.len()
}

// BuildSpansJSON builds the OTLP JSON payload for traces
// Returns the length of the payload in BodyBuf
func BuildSpansJSON() int {
	// Count valid completed spans (non-zero trace ID)
	completedCount := 0
	for i := 0; i < len(SpanQueue); i++ {
		span := &SpanQueue[i]
		if span.Active || span.EndTime == 0 {
			continue
		}
		// Check for valid trace ID
		for _, b := range span.TraceID {
			if b != 0 {
				completedCount++
				break
			}
		}
	}

	if completedCount == 0 {
		return 0
	}

	var w jsonWriter
	w.reset()

	// Start resourceSpans
	w.writeRaw(`{"resourceSpans":[{`)
	w.writeResourceAttributes()
	w.writeRaw(`,"scopeSpans":[{"scope":{"name":"bindicator"},"spans":[`)

	// Write each completed span
	first := true
	for i := 0; i < len(SpanQueue); i++ {
		span := &SpanQueue[i]
		if span.Active || span.EndTime == 0 {
			continue
		}

		// Skip spans with zero trace ID (invalid)
		traceIDZero := true
		for _, b := range span.TraceID {
			if b != 0 {
				traceIDZero = false
				break
			}
		}
		if traceIDZero {
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

		// Parent span ID if not zero
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
		w.writeRaw(`,"kind":`)
		w.writeInt(int(span.Kind))
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

		// Clear the span after sending
		span.EndTime = 0
	}

	// Close JSON
	w.writeRaw(`]}]}]}`)

	return w.len()
}
