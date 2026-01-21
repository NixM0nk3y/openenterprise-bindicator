package main

import "testing"

func TestAtoi2(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"00", 0},
		{"01", 1},
		{"09", 9},
		{"10", 10},
		{"12", 12},
		{"31", 31},
		{"99", 99},
		// Edge cases
		{"", 0},
		{"1", 0},
		{"abc", 0},
	}

	for _, tc := range tests {
		got := atoi2([]byte(tc.input))
		if got != tc.expected {
			t.Errorf("atoi2(%q) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}

func TestAtoi4(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"0000", 0},
		{"0001", 1},
		{"2024", 2024},
		{"2026", 2026},
		{"1999", 1999},
		{"9999", 9999},
		// Edge cases
		{"", 0},
		{"123", 0},
		{"abcd", 0},
	}

	for _, tc := range tests {
		got := atoi4([]byte(tc.input))
		if got != tc.expected {
			t.Errorf("atoi4(%q) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}

func TestParseBinType(t *testing.T) {
	tests := []struct {
		input    string
		expected BinType
	}{
		{"GREEN", BinGreen},
		{"green", BinGreen},
		{"Green", BinGreen},
		{"BLACK", BinBlack},
		{"black", BinBlack},
		{"Black", BinBlack},
		{"BROWN", BinBrown},
		{"brown", BinBrown},
		{"Brown", BinBrown},
		// Edge cases
		{"", BinUnknown},
		{"RED", BinUnknown},
		{"G", BinUnknown},
		{"GREE", BinUnknown},
	}

	for _, tc := range tests {
		got := parseBinType([]byte(tc.input))
		if got != tc.expected {
			t.Errorf("parseBinType(%q) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}

func TestParseScheduleResponse(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedCount  int
		expectedTS     int64
		expectedJobs   []BinJob
	}{
		{
			name:          "empty",
			input:         "",
			expectedCount: 0,
			expectedTS:    0,
			expectedJobs:  nil,
		},
		{
			name:          "timestamp only",
			input:         "1737207000",
			expectedCount: 0,
			expectedTS:    1737207000,
			expectedJobs:  nil,
		},
		{
			name:          "single job",
			input:         "1737207000,2026-01-17:BLACK",
			expectedCount: 1,
			expectedTS:    1737207000,
			expectedJobs: []BinJob{
				{Year: 2026, Month: 1, Day: 17, Bin: BinBlack},
			},
		},
		{
			name:          "multiple jobs",
			input:         "1737207000,2026-01-17:BLACK,2026-01-31:GREEN,2026-02-14:BROWN",
			expectedCount: 3,
			expectedTS:    1737207000,
			expectedJobs: []BinJob{
				{Year: 2026, Month: 1, Day: 17, Bin: BinBlack},
				{Year: 2026, Month: 1, Day: 31, Bin: BinGreen},
				{Year: 2026, Month: 2, Day: 14, Bin: BinBrown},
			},
		},
		{
			name:          "lowercase bin types",
			input:         "1000000000,2026-03-01:green,2026-03-08:black",
			expectedCount: 2,
			expectedTS:    1000000000,
			expectedJobs: []BinJob{
				{Year: 2026, Month: 3, Day: 1, Bin: BinGreen},
				{Year: 2026, Month: 3, Day: 8, Bin: BinBlack},
			},
		},
		{
			name:          "invalid job skipped",
			input:         "1234567890,2026-01-15:BLACK,invalid,2026-01-22:GREEN",
			expectedCount: 2,
			expectedTS:    1234567890,
			expectedJobs: []BinJob{
				{Year: 2026, Month: 1, Day: 15, Bin: BinBlack},
				{Year: 2026, Month: 1, Day: 22, Bin: BinGreen},
			},
		},
		{
			name:          "unknown bin type skipped",
			input:         "1234567890,2026-01-15:RED,2026-01-22:GREEN",
			expectedCount: 1,
			expectedTS:    1234567890,
			expectedJobs: []BinJob{
				{Year: 2026, Month: 1, Day: 22, Bin: BinGreen},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := parseScheduleResponse([]byte(tc.input))

			if count != tc.expectedCount {
				t.Errorf("count = %d, want %d", count, tc.expectedCount)
			}

			if parsedTimestamp != tc.expectedTS {
				t.Errorf("parsedTimestamp = %d, want %d", parsedTimestamp, tc.expectedTS)
			}

			jobs := getJobs()
			if len(jobs) != len(tc.expectedJobs) {
				t.Errorf("len(jobs) = %d, want %d", len(jobs), len(tc.expectedJobs))
				return
			}

			for i, expected := range tc.expectedJobs {
				got := jobs[i]
				if got.Year != expected.Year || got.Month != expected.Month ||
					got.Day != expected.Day || got.Bin != expected.Bin {
					t.Errorf("job[%d] = %+v, want %+v", i, got, expected)
				}
			}
		})
	}
}

func TestParseScheduleResponseMaxJobs(t *testing.T) {
	// Build input with more than maxJobs entries
	input := "1234567890"
	for i := 0; i < 20; i++ {
		input += ",2026-01-15:BLACK"
	}

	count := parseScheduleResponse([]byte(input))

	if count != maxJobs {
		t.Errorf("count = %d, want %d (maxJobs)", count, maxJobs)
	}
}
