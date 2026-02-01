//go:build !tinygo

package main

import "time"

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
