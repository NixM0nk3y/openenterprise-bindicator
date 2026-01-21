//go:build !tinygo

package main

import "time"

// BinType represents the type of bin collection
type BinType uint8

const (
	BinUnknown BinType = iota
	BinGreen
	BinBlack
	BinBrown
)

// BinJob represents a scheduled bin collection
type BinJob struct {
	Year  uint16
	Month uint8
	Day   uint8
	Bin   BinType
}

// Pre-allocated storage for bin jobs (avoids heap allocation)
const maxJobs = 15

var (
	jobStorage [maxJobs]BinJob
	jobCount   int
)

// getJobs returns the current job storage slice
func getJobs() []BinJob {
	return jobStorage[:jobCount]
}

// clearJobs resets the job count
func clearJobs() {
	jobCount = 0
}

// LED state storage (for testing)
var ledState struct {
	green bool
	black bool
	brown bool
}

// isInCollectionWindow checks if the given time is within the LED window for a job.
// Window: noon day before to noon on collection day.
func isInCollectionWindow(job BinJob, now time.Time) bool {
	collectionDate := time.Date(
		int(job.Year), time.Month(job.Month), int(job.Day),
		0, 0, 0, 0, time.UTC,
	)
	windowStart := collectionDate.Add(-12 * time.Hour)
	windowEnd := collectionDate.Add(12 * time.Hour)
	return now.After(windowStart) && now.Before(windowEnd)
}

// updateLEDsFromSchedule checks the schedule and updates LED states.
// This is a test-compatible version without hardware dependencies.
func updateLEDsFromSchedule(jobs []BinJob, now time.Time) {
	ledState.green = false
	ledState.black = false
	ledState.brown = false

	for i := 0; i < len(jobs); i++ {
		if isInCollectionWindow(jobs[i], now) {
			switch jobs[i].Bin {
			case BinGreen:
				ledState.green = true
			case BinBlack:
				ledState.black = true
			case BinBrown:
				ledState.brown = true
			}
		}
	}
}
