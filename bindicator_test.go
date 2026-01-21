package main

import (
	"testing"
	"time"
)

func TestIsInCollectionWindow(t *testing.T) {
	// Collection date: 2026-01-20 (Monday)
	// Window: 2026-01-19 12:00 to 2026-01-20 12:00
	job := BinJob{Year: 2026, Month: 1, Day: 20, Bin: BinBlack}

	tests := []struct {
		name     string
		time     time.Time
		expected bool
	}{
		// Before window
		{
			name:     "day before morning",
			time:     time.Date(2026, 1, 19, 8, 0, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "day before 11:59",
			time:     time.Date(2026, 1, 19, 11, 59, 0, 0, time.UTC),
			expected: false,
		},
		// Window boundary start (exactly noon is NOT in window due to After())
		{
			name:     "day before noon exactly",
			time:     time.Date(2026, 1, 19, 12, 0, 0, 0, time.UTC),
			expected: false,
		},
		// Inside window
		{
			name:     "day before 12:01",
			time:     time.Date(2026, 1, 19, 12, 1, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "day before evening",
			time:     time.Date(2026, 1, 19, 20, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "collection day midnight",
			time:     time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "collection day morning",
			time:     time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "collection day 11:59",
			time:     time.Date(2026, 1, 20, 11, 59, 0, 0, time.UTC),
			expected: true,
		},
		// Window boundary end (exactly noon is NOT in window due to Before())
		{
			name:     "collection day noon exactly",
			time:     time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC),
			expected: false,
		},
		// After window
		{
			name:     "collection day 12:01",
			time:     time.Date(2026, 1, 20, 12, 1, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "collection day evening",
			time:     time.Date(2026, 1, 20, 20, 0, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "day after",
			time:     time.Date(2026, 1, 21, 8, 0, 0, 0, time.UTC),
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isInCollectionWindow(job, tc.time)
			if got != tc.expected {
				t.Errorf("isInCollectionWindow() at %v = %v, want %v",
					tc.time.Format("2006-01-02 15:04"), got, tc.expected)
			}
		})
	}
}

func TestUpdateLEDsFromSchedule(t *testing.T) {
	tests := []struct {
		name          string
		jobs          []BinJob
		now           time.Time
		expectGreen   bool
		expectBlack   bool
		expectBrown   bool
	}{
		{
			name:          "no jobs",
			jobs:          []BinJob{},
			now:           time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expectGreen:   false,
			expectBlack:   false,
			expectBrown:   false,
		},
		{
			name: "single job in window",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
			},
			now:           time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expectGreen:   false,
			expectBlack:   true,
			expectBrown:   false,
		},
		{
			name: "single job not in window",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
			},
			now:           time.Date(2026, 1, 18, 8, 0, 0, 0, time.UTC),
			expectGreen:   false,
			expectBlack:   false,
			expectBrown:   false,
		},
		{
			name: "multiple jobs same day",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
				{Year: 2026, Month: 1, Day: 20, Bin: BinGreen},
			},
			now:           time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expectGreen:   true,
			expectBlack:   true,
			expectBrown:   false,
		},
		{
			name: "multiple jobs different days - one in window",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 15, Bin: BinGreen},
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
				{Year: 2026, Month: 1, Day: 25, Bin: BinBrown},
			},
			now:           time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expectGreen:   false,
			expectBlack:   true,
			expectBrown:   false,
		},
		{
			name: "all three bins in window",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 20, Bin: BinGreen},
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
				{Year: 2026, Month: 1, Day: 20, Bin: BinBrown},
			},
			now:           time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			expectGreen:   true,
			expectBlack:   true,
			expectBrown:   true,
		},
		{
			name: "evening before collection",
			jobs: []BinJob{
				{Year: 2026, Month: 1, Day: 20, Bin: BinBlack},
			},
			now:           time.Date(2026, 1, 19, 20, 0, 0, 0, time.UTC),
			expectGreen:   false,
			expectBlack:   true,
			expectBrown:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset state
			ledState.green = false
			ledState.black = false
			ledState.brown = false

			updateLEDsFromSchedule(tc.jobs, tc.now)

			if ledState.green != tc.expectGreen {
				t.Errorf("green LED = %v, want %v", ledState.green, tc.expectGreen)
			}
			if ledState.black != tc.expectBlack {
				t.Errorf("black LED = %v, want %v", ledState.black, tc.expectBlack)
			}
			if ledState.brown != tc.expectBrown {
				t.Errorf("brown LED = %v, want %v", ledState.brown, tc.expectBrown)
			}
		})
	}
}

func TestWindowEdgeCases(t *testing.T) {
	// Test month/year boundaries
	tests := []struct {
		name     string
		job      BinJob
		now      time.Time
		expected bool
	}{
		{
			name:     "new year boundary - Dec 31 evening for Jan 1 collection",
			job:      BinJob{Year: 2026, Month: 1, Day: 1, Bin: BinBlack},
			now:      time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "new year boundary - Dec 31 morning not in window",
			job:      BinJob{Year: 2026, Month: 1, Day: 1, Bin: BinBlack},
			now:      time.Date(2025, 12, 31, 8, 0, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "month boundary - Jan 31 evening for Feb 1 collection",
			job:      BinJob{Year: 2026, Month: 2, Day: 1, Bin: BinGreen},
			now:      time.Date(2026, 1, 31, 20, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "leap year Feb 29",
			job:      BinJob{Year: 2028, Month: 2, Day: 29, Bin: BinBrown},
			now:      time.Date(2028, 2, 29, 8, 0, 0, 0, time.UTC),
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isInCollectionWindow(tc.job, tc.now)
			if got != tc.expected {
				t.Errorf("isInCollectionWindow() = %v, want %v", got, tc.expected)
			}
		})
	}
}
