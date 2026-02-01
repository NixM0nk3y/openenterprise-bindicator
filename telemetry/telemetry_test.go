package telemetry

import (
	"strings"
	"testing"
)

func TestLog(t *testing.T) {
	ResetState()

	tests := []struct {
		name     string
		severity uint8
		msg      string
	}{
		{"debug message", SeverityDebug, "debug:test"},
		{"info message", SeverityInfo, "info:test"},
		{"warn message", SeverityWarn, "warn:test"},
		{"error message", SeverityError, "error:test"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ResetState()
			Log(tc.severity, tc.msg)

			logs := GetLogQueue()
			if len(logs) != 1 {
				t.Fatalf("expected 1 log, got %d", len(logs))
			}

			log := logs[0]
			if log.Severity != tc.severity {
				t.Errorf("severity = %d, want %d", log.Severity, tc.severity)
			}

			body := string(log.Body[:log.BodyLen])
			if body != tc.msg {
				t.Errorf("body = %q, want %q", body, tc.msg)
			}

			if log.Timestamp == 0 {
				t.Error("timestamp should not be zero")
			}
		})
	}
}

func TestLogConvenienceFunctions(t *testing.T) {
	tests := []struct {
		name     string
		logFunc  func(string)
		expected uint8
	}{
		{"LogDebug", LogDebug, SeverityDebug},
		{"LogInfo", LogInfo, SeverityInfo},
		{"LogWarn", LogWarn, SeverityWarn},
		{"LogError", LogError, SeverityError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ResetState()
			tc.logFunc("test message")

			logs := GetLogQueue()
			if len(logs) != 1 {
				t.Fatalf("expected 1 log, got %d", len(logs))
			}

			if logs[0].Severity != tc.expected {
				t.Errorf("severity = %d, want %d", logs[0].Severity, tc.expected)
			}
		})
	}
}

func TestLogQueueCircular(t *testing.T) {
	ResetState()

	// Fill queue beyond capacity (queue size is 8)
	for i := 0; i < 12; i++ {
		LogInfo("message")
	}

	logs := GetLogQueue()
	if len(logs) != 8 {
		t.Errorf("queue length = %d, want 8 (max)", len(logs))
	}
}

func TestLogTruncation(t *testing.T) {
	ResetState()

	// Message longer than 64 bytes
	longMsg := strings.Repeat("x", 100)
	LogInfo(longMsg)

	logs := GetLogQueue()
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}

	if logs[0].BodyLen != 64 {
		t.Errorf("bodyLen = %d, want 64 (truncated)", logs[0].BodyLen)
	}
}

func TestLogDisabled(t *testing.T) {
	ResetState()
	Disable()

	LogInfo("should not be queued")

	logs := GetLogQueue()
	if len(logs) != 0 {
		t.Errorf("expected 0 logs when disabled, got %d", len(logs))
	}

	Enable()
}

func TestLogWithTraceContext(t *testing.T) {
	ResetState()

	// Set trace context
	var traceID [16]byte
	var spanID [8]byte
	for i := 0; i < 16; i++ {
		traceID[i] = byte(i + 1)
	}
	for i := 0; i < 8; i++ {
		spanID[i] = byte(i + 10)
	}
	SetTraceContext(traceID, spanID)

	LogInfo("with trace")

	logs := GetLogQueue()
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}

	log := logs[0]
	if !log.HasTrace {
		t.Error("expected HasTrace = true")
	}

	if log.TraceID != traceID {
		t.Error("traceID mismatch")
	}

	if log.SpanID != spanID {
		t.Error("spanID mismatch")
	}
}

func TestRecordGauge(t *testing.T) {
	ResetState()

	RecordGauge("temperature", 25)

	metrics := GetMetricQueue()
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}

	m := metrics[0]
	name := string(m.Name[:m.NameLen])
	if name != "temperature" {
		t.Errorf("name = %q, want %q", name, "temperature")
	}

	if m.Value != 25 {
		t.Errorf("value = %d, want 25", m.Value)
	}

	if !m.IsGauge {
		t.Error("expected IsGauge = true")
	}

	if m.Timestamp == 0 {
		t.Error("timestamp should not be zero")
	}
}

func TestRecordCounter(t *testing.T) {
	ResetState()

	RecordCounter("requests.total", 100)

	metrics := GetMetricQueue()
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}

	m := metrics[0]
	name := string(m.Name[:m.NameLen])
	if name != "requests.total" {
		t.Errorf("name = %q, want %q", name, "requests.total")
	}

	if m.Value != 100 {
		t.Errorf("value = %d, want 100", m.Value)
	}

	if m.IsGauge {
		t.Error("expected IsGauge = false for counter")
	}
}

func TestMetricQueueCircular(t *testing.T) {
	ResetState()

	// Fill queue beyond capacity (queue size is 8)
	for i := 0; i < 12; i++ {
		RecordGauge("metric", int64(i))
	}

	metrics := GetMetricQueue()
	if len(metrics) != 8 {
		t.Errorf("queue length = %d, want 8 (max)", len(metrics))
	}

	// Oldest entries should be overwritten (values 0-3 gone, 4-11 remain)
	if metrics[0].Value != 4 {
		t.Errorf("oldest metric value = %d, want 4", metrics[0].Value)
	}
}

func TestMetricNameTruncation(t *testing.T) {
	ResetState()

	// Name longer than 32 bytes
	longName := strings.Repeat("x", 50)
	RecordGauge(longName, 42)

	metrics := GetMetricQueue()
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}

	if metrics[0].NameLen != 32 {
		t.Errorf("nameLen = %d, want 32 (truncated)", metrics[0].NameLen)
	}
}

func TestSpanLifecycle(t *testing.T) {
	ResetState()

	// Set trace context first
	var traceID [16]byte
	for i := 0; i < 16; i++ {
		traceID[i] = byte(i + 1)
	}
	SetTraceContext(traceID, [8]byte{})

	// Start span
	idx := StartSpanTest("test-operation")
	if idx < 0 {
		t.Fatal("StartSpanTest returned invalid index")
	}

	// Span should be active (not yet in completed list)
	spans := GetSpanQueue()
	if len(spans) != 0 {
		t.Errorf("expected 0 completed spans while active, got %d", len(spans))
	}

	// End span successfully
	EndSpan(idx, true)

	spans = GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 completed span, got %d", len(spans))
	}

	span := spans[0]
	name := string(span.Name[:span.NameLen])
	if name != "test-operation" {
		t.Errorf("span name = %q, want %q", name, "test-operation")
	}

	if !span.StatusOK {
		t.Error("expected StatusOK = true")
	}

	if span.StartTime == 0 {
		t.Error("StartTime should not be zero")
	}

	if span.EndTime == 0 {
		t.Error("EndTime should not be zero")
	}

	if span.EndTime < span.StartTime {
		t.Error("EndTime should be >= StartTime")
	}

	if span.TraceID != traceID {
		t.Error("traceID mismatch")
	}
}

func TestSpanFailedStatus(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	idx := StartSpanTest("failing-op")
	EndSpan(idx, false)

	spans := GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	if spans[0].StatusOK {
		t.Error("expected StatusOK = false for failed span")
	}
}

func TestSpanInvalidIndex(t *testing.T) {
	ResetState()

	// Should not panic with invalid index
	EndSpan(-1, true)
	EndSpan(100, true)

	spans := GetSpanQueue()
	if len(spans) != 0 {
		t.Errorf("expected 0 spans, got %d", len(spans))
	}
}

func TestSpanNameTruncation(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1}, [8]byte{})

	longName := strings.Repeat("x", 50)
	idx := StartSpanTest(longName)
	EndSpan(idx, true)

	spans := GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	if spans[0].NameLen != 32 {
		t.Errorf("nameLen = %d, want 32 (truncated)", spans[0].NameLen)
	}
}

func TestDisabledMetrics(t *testing.T) {
	ResetState()
	Disable()

	RecordGauge("test", 42)

	metrics := GetMetricQueue()
	if len(metrics) != 0 {
		t.Errorf("expected 0 metrics when disabled, got %d", len(metrics))
	}

	Enable()
}

func TestDisabledSpans(t *testing.T) {
	ResetState()
	Disable()

	idx := StartSpanTest("test")
	if idx != -1 {
		t.Errorf("StartSpanTest should return -1 when disabled, got %d", idx)
	}

	Enable()
}

func TestSeverityConstants(t *testing.T) {
	// Verify OTLP severity numbers match expected values
	if SeverityDebug != 5 {
		t.Errorf("SeverityDebug = %d, want 5", SeverityDebug)
	}
	if SeverityInfo != 9 {
		t.Errorf("SeverityInfo = %d, want 9", SeverityInfo)
	}
	if SeverityWarn != 13 {
		t.Errorf("SeverityWarn = %d, want 13", SeverityWarn)
	}
	if SeverityError != 17 {
		t.Errorf("SeverityError = %d, want 17", SeverityError)
	}
}

func TestSpanStatusConstants(t *testing.T) {
	// Verify OTLP status codes
	if SpanStatusUnset != 0 {
		t.Errorf("SpanStatusUnset = %d, want 0", SpanStatusUnset)
	}
	if SpanStatusOK != 1 {
		t.Errorf("SpanStatusOK = %d, want 1", SpanStatusOK)
	}
	if SpanStatusError != 2 {
		t.Errorf("SpanStatusError = %d, want 2", SpanStatusError)
	}
}

func TestSpanPendingPreventsReuse(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	// Start and end span A
	idxA := StartSpanTest("span-a")
	EndSpan(idxA, true)

	// Span A should be pending (not yet flushed)
	if GetPendingSpanCount() != 1 {
		t.Fatalf("expected 1 pending span, got %d", GetPendingSpanCount())
	}

	// Start span B - should NOT reuse span A's slot
	idxB := StartSpanTest("span-b")
	if idxB == idxA {
		t.Error("span B should not reuse span A's slot while A is pending")
	}

	// Both spans should exist
	EndSpan(idxB, true)
	spans := GetSpanQueue()
	if len(spans) != 2 {
		t.Errorf("expected 2 spans, got %d", len(spans))
	}

	// Verify both span names exist
	names := make(map[string]bool)
	for _, s := range spans {
		names[string(s.Name[:s.NameLen])] = true
	}
	if !names["span-a"] || !names["span-b"] {
		t.Errorf("expected span-a and span-b, got %v", names)
	}
}

func TestSpanFlushAllowsReuse(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	// Start and end span A
	idxA := StartSpanTest("span-a")
	EndSpan(idxA, true)

	// Flush spans (simulates 30s flush interval)
	FlushSpans()

	if GetPendingSpanCount() != 0 {
		t.Fatalf("expected 0 pending spans after flush, got %d", GetPendingSpanCount())
	}

	// Start span B - should now be able to reuse span A's slot
	idxB := StartSpanTest("span-b")
	if idxB != idxA {
		t.Errorf("span B should reuse span A's slot after flush, got idx %d want %d", idxB, idxA)
	}
}

func TestSpanNestedParentChild(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	// Record initial span ID (root)
	rootSpanID := GetCurrentSpanID()

	// Start parent span
	parentIdx := StartSpanTest("parent")
	parentSpanID := GetCurrentSpanID()

	// Parent's parent should be the root
	parentSpan := SpanQueue[parentIdx]
	if parentSpan.ParentID != rootSpanID {
		t.Error("parent span's ParentID should be root span ID")
	}

	// Start child span
	childIdx := StartSpanTest("child")
	childSpanID := GetCurrentSpanID()

	// Child's parent should be the parent span
	childSpan := SpanQueue[childIdx]
	if childSpan.ParentID != parentSpanID {
		t.Error("child span's ParentID should be parent span ID")
	}

	// End child - current span should revert to parent
	EndSpan(childIdx, true)
	if GetCurrentSpanID() != parentSpanID {
		t.Error("after ending child, current span should be parent")
	}

	// End parent - current span should revert to root
	EndSpan(parentIdx, true)
	if GetCurrentSpanID() != rootSpanID {
		t.Error("after ending parent, current span should be root")
	}

	// Verify we have both spans
	spans := GetSpanQueue()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	// Verify parent-child relationship in completed spans
	var foundParent, foundChild bool
	for _, s := range spans {
		name := string(s.Name[:s.NameLen])
		if name == "parent" {
			foundParent = true
			if s.SpanID != parentSpanID {
				t.Error("parent span ID mismatch")
			}
		}
		if name == "child" {
			foundChild = true
			if s.ParentID != parentSpanID {
				t.Error("child's ParentID should match parent's SpanID")
			}
			if s.SpanID != childSpanID {
				t.Error("child span ID mismatch")
			}
		}
	}
	if !foundParent || !foundChild {
		t.Error("missing parent or child span")
	}
}

func TestSpanSiblings(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	rootSpanID := GetCurrentSpanID()

	// Start parent
	parentIdx := StartSpanTest("parent")
	parentSpanID := GetCurrentSpanID()

	// Start first child
	child1Idx := StartSpanTest("child-1")

	// End first child - should restore to parent
	EndSpan(child1Idx, true)
	if GetCurrentSpanID() != parentSpanID {
		t.Error("after ending child-1, current span should be parent")
	}

	// Start second child (sibling of first)
	child2Idx := StartSpanTest("child-2")

	// Second child's parent should also be parent span (not child-1)
	child2Span := SpanQueue[child2Idx]
	if child2Span.ParentID != parentSpanID {
		t.Error("child-2's ParentID should be parent, not child-1")
	}

	// Clean up
	EndSpan(child2Idx, true)
	EndSpan(parentIdx, true)

	if GetCurrentSpanID() != rootSpanID {
		t.Error("after ending all spans, should be back to root")
	}

	// Verify all 3 spans exist with correct parents
	spans := GetSpanQueue()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}

	childCount := 0
	for _, s := range spans {
		name := string(s.Name[:s.NameLen])
		if strings.HasPrefix(name, "child-") {
			childCount++
			if s.ParentID != parentSpanID {
				t.Errorf("%s should have parent as ParentID", name)
			}
		}
	}
	if childCount != 2 {
		t.Errorf("expected 2 child spans, got %d", childCount)
	}
}

func TestSpanQueueOverflow(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	// Queue size is 4, start 4 spans without ending them
	indices := make([]int, 4)
	for i := 0; i < 4; i++ {
		indices[i] = StartSpanTest("span")
	}

	if GetActiveSpanCount() != 4 {
		t.Fatalf("expected 4 active spans, got %d", GetActiveSpanCount())
	}

	// Starting a 5th span should reuse the oldest slot (circular queue)
	idx5 := StartSpanTest("overflow")

	// Should have overwritten slot 0 (oldest)
	if idx5 != 0 {
		t.Errorf("overflow span should use slot 0, got %d", idx5)
	}

	// Clean up
	for _, idx := range indices[1:] { // Skip index 0 which was overwritten
		EndSpan(idx, true)
	}
	EndSpan(idx5, true)
}

func TestSpanQueueMixedActiveAndPending(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	// Start span 0 and end it (pending)
	idx0 := StartSpanTest("pending-0")
	EndSpan(idx0, true)

	// Start span 1 and end it (pending)
	idx1 := StartSpanTest("pending-1")
	EndSpan(idx1, true)

	// Start span 2 (active)
	idx2 := StartSpanTest("active-2")

	// Start span 3 (active)
	idx3 := StartSpanTest("active-3")

	// All 4 slots are now in use (2 pending, 2 active)
	if GetPendingSpanCount() != 2 {
		t.Errorf("expected 2 pending spans, got %d", GetPendingSpanCount())
	}
	if GetActiveSpanCount() != 2 {
		t.Errorf("expected 2 active spans, got %d", GetActiveSpanCount())
	}

	// Starting another span should use circular queue (overwrite oldest)
	idx4 := StartSpanTest("overflow")
	// Should overwrite slot 0 (oldest, even though pending)
	if idx4 != 0 {
		t.Errorf("expected overflow to use slot 0, got %d", idx4)
	}

	// Clean up
	EndSpan(idx2, true)
	EndSpan(idx3, true)
	EndSpan(idx4, true)
}

func TestSetSpanStatus(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	idx := StartSpanTest("test-op")
	SetSpanStatus(idx, "success:result")
	EndSpan(idx, true)

	spans := GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	msg := string(spans[0].StatusMsg[:spans[0].StatusLen])
	if msg != "success:result" {
		t.Errorf("status message = %q, want %q", msg, "success:result")
	}
}

func TestSetSpanStatusTruncation(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	idx := StartSpanTest("test-op")

	// Create a message longer than the 48-byte buffer
	longMsg := strings.Repeat("x", 100)
	SetSpanStatus(idx, longMsg)
	EndSpan(idx, true)

	spans := GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	// Should be truncated to 48 bytes
	if spans[0].StatusLen != 48 {
		t.Errorf("status length = %d, want 48 (truncated)", spans[0].StatusLen)
	}

	msg := string(spans[0].StatusMsg[:spans[0].StatusLen])
	expected := strings.Repeat("x", 48)
	if msg != expected {
		t.Errorf("status message = %q, want %q", msg, expected)
	}
}

func TestSetSpanStatusOnInactiveSpan(t *testing.T) {
	ResetState()
	SetTraceContext([16]byte{1, 2, 3}, [8]byte{})

	idx := StartSpanTest("test-op")
	EndSpan(idx, true)

	// Try to set status on already-ended span (should be ignored)
	SetSpanStatus(idx, "should-be-ignored")

	spans := GetSpanQueue()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	// Status should be empty (was set after EndSpan)
	if spans[0].StatusLen != 0 {
		t.Errorf("status length = %d, want 0 (should not be set after EndSpan)", spans[0].StatusLen)
	}
}

func TestSetSpanStatusInvalidIndex(t *testing.T) {
	ResetState()

	// Should not panic with invalid index
	SetSpanStatus(-1, "test")
	SetSpanStatus(100, "test")
}
