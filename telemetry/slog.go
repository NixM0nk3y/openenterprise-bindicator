//go:build tinygo

package telemetry

import (
	"context"
	"io"
	"log/slog"
)

// SlogHandler is a slog.Handler that bridges logs to both
// the console (via TextHandler) and the OpenTelemetry telemetry system.
type SlogHandler struct {
	textHandler slog.Handler
	level       slog.Leveler
	attrs       []slog.Attr
	group       string
}

// NewSlogHandler creates a new handler that writes to the given
// writer (typically machine.Serial) and also queues logs to telemetry.
func NewSlogHandler(w io.Writer, opts *slog.HandlerOptions) *SlogHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	return &SlogHandler{
		textHandler: slog.NewTextHandler(w, opts),
		level:       opts.Level,
	}
}

// Enabled reports whether the handler handles records at the given level.
func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.textHandler.Enabled(ctx, level)
}

// Handle handles the Record by writing to both the console and telemetry.
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	// Always write to console via text handler
	err := h.textHandler.Handle(ctx, r)

	// Only queue INFO and above to telemetry (skip DEBUG to save buffer space)
	if r.Level >= slog.LevelInfo {
		msg := buildTelemetryMessage(h.group, r)
		severity := slogLevelToOTLP(r.Level)
		Log(severity, msg)
	}

	return err
}

// WithAttrs returns a new Handler with the given attributes added.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)

	return &SlogHandler{
		textHandler: h.textHandler.WithAttrs(attrs),
		level:       h.level,
		attrs:       newAttrs,
		group:       h.group,
	}
}

// WithGroup returns a new Handler with the given group name.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}

	return &SlogHandler{
		textHandler: h.textHandler.WithGroup(name),
		level:       h.level,
		attrs:       h.attrs,
		group:       newGroup,
	}
}

// slogLevelToOTLP converts slog.Level to OTLP severity number
func slogLevelToOTLP(level slog.Level) uint8 {
	switch {
	case level >= slog.LevelError:
		return SeverityError // 17
	case level >= slog.LevelWarn:
		return SeverityWarn // 13
	case level >= slog.LevelInfo:
		return SeverityInfo // 9
	default:
		return SeverityDebug // 5
	}
}

// buildTelemetryMessage builds a compact message string for telemetry
// Format: "msg" or "msg key=val key2=val2" (truncated to fit)
func buildTelemetryMessage(group string, r slog.Record) string {
	// Pre-allocated buffer for message building
	var buf [128]byte
	pos := 0

	// Add group prefix if present
	if group != "" {
		pos = copyToBuffer(buf[:], pos, group)
		if pos < len(buf) {
			buf[pos] = ':'
			pos++
		}
	}

	// Add message
	pos = copyToBuffer(buf[:], pos, r.Message)

	// Add first few attributes if space permits
	attrCount := 0
	r.Attrs(func(a slog.Attr) bool {
		if attrCount >= 4 || pos >= len(buf)-10 {
			return false // Stop after 4 attrs or if running out of space
		}

		// Add separator
		if pos < len(buf) {
			buf[pos] = ' '
			pos++
		}

		// Add key=value
		pos = copyToBuffer(buf[:], pos, a.Key)
		if pos < len(buf) {
			buf[pos] = '='
			pos++
		}
		pos = copyAttrValue(buf[:], pos, a.Value)

		attrCount++
		return true
	})

	return string(buf[:pos])
}

// copyToBuffer copies a string to the buffer, returns new position
func copyToBuffer(buf []byte, pos int, s string) int {
	for i := 0; i < len(s) && pos < len(buf); i++ {
		buf[pos] = s[i]
		pos++
	}
	return pos
}

// copyAttrValue copies an attribute value to the buffer
func copyAttrValue(buf []byte, pos int, v slog.Value) int {
	switch v.Kind() {
	case slog.KindString:
		return copyToBuffer(buf, pos, v.String())
	case slog.KindInt64:
		return copyInt64ToBuffer(buf, pos, v.Int64())
	case slog.KindUint64:
		return copyUint64ToBuffer(buf, pos, v.Uint64())
	case slog.KindBool:
		if v.Bool() {
			return copyToBuffer(buf, pos, "true")
		}
		return copyToBuffer(buf, pos, "false")
	case slog.KindDuration:
		return copyDurationToBuffer(buf, pos, int64(v.Duration()))
	case slog.KindFloat64:
		// Simple integer representation for floats
		return copyInt64ToBuffer(buf, pos, int64(v.Float64()))
	default:
		return copyToBuffer(buf, pos, "?")
	}
}

// copyInt64ToBuffer copies an int64 to the buffer as decimal string
func copyInt64ToBuffer(buf []byte, pos int, n int64) int {
	if n == 0 {
		if pos < len(buf) {
			buf[pos] = '0'
			return pos + 1
		}
		return pos
	}

	if n < 0 {
		if pos < len(buf) {
			buf[pos] = '-'
			pos++
		}
		n = -n
	}

	return copyUint64ToBuffer(buf, pos, uint64(n))
}

// copyUint64ToBuffer copies a uint64 to the buffer as decimal string
func copyUint64ToBuffer(buf []byte, pos int, n uint64) int {
	if n == 0 {
		if pos < len(buf) {
			buf[pos] = '0'
			return pos + 1
		}
		return pos
	}

	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}

	for j := i; j < len(digits) && pos < len(buf); j++ {
		buf[pos] = digits[j]
		pos++
	}
	return pos
}

// copyDurationToBuffer copies a duration in a compact format (e.g., "5s", "100ms")
func copyDurationToBuffer(buf []byte, pos int, d int64) int {
	// Duration is in nanoseconds
	if d == 0 {
		return copyToBuffer(buf, pos, "0s")
	}

	// Convert to most appropriate unit
	if d >= 1e9 {
		// Seconds
		pos = copyInt64ToBuffer(buf, pos, d/1e9)
		return copyToBuffer(buf, pos, "s")
	} else if d >= 1e6 {
		// Milliseconds
		pos = copyInt64ToBuffer(buf, pos, d/1e6)
		return copyToBuffer(buf, pos, "ms")
	} else if d >= 1e3 {
		// Microseconds
		pos = copyInt64ToBuffer(buf, pos, d/1e3)
		return copyToBuffer(buf, pos, "us")
	}
	// Nanoseconds
	pos = copyInt64ToBuffer(buf, pos, d)
	return copyToBuffer(buf, pos, "ns")
}
