//go:build tinygo

package main

import (
	"log/slog"
	"machine"
	"time"
)

// Package-level logger for bindicator (set from main)
var bindicatorLogger *slog.Logger

// GPIO pin assignments for external LEDs
const (
	pinGreenLED = machine.GP2
	pinBlackLED = machine.GP3
	pinBrownLED = machine.GP4
)

// BinType represents the type of bin collection
type BinType uint8

const (
	BinUnknown BinType = iota
	BinGreen
	BinBlack
	BinBrown
)

// String returns the bin type name
func (b BinType) String() string {
	switch b {
	case BinGreen:
		return "green"
	case BinBlack:
		return "black"
	case BinBrown:
		return "brown"
	default:
		return "unknown"
	}
}

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

// LED state storage (persists across API errors)
var ledState struct {
	green bool
	black bool
	brown bool
}

// bindicatorPaused stops LED updates during OTA
var bindicatorPaused bool

// SetBindicatorPaused pauses/resumes bindicator LED updates
func SetBindicatorPaused(p bool) {
	bindicatorPaused = p
}

// IsBindicatorPaused returns true if bindicator is paused
func IsBindicatorPaused() bool {
	return bindicatorPaused
}

// initLEDs configures the GPIO pins for LED output
func initLEDs() {
	pinGreenLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	pinBlackLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	pinBrownLED.Configure(machine.PinConfig{Mode: machine.PinOutput})

	// Initialize all LEDs off
	pinGreenLED.Low()
	pinBlackLED.Low()
	pinBrownLED.Low()
}

// setLED sets the state of a specific bin LED
func setLED(binType BinType, on bool) {
	var changed bool
	var name string

	switch binType {
	case BinGreen:
		changed = ledState.green != on
		name = "GREEN"
		if on {
			pinGreenLED.High()
		} else {
			pinGreenLED.Low()
		}
		ledState.green = on
	case BinBlack:
		changed = ledState.black != on
		name = "BLACK"
		if on {
			pinBlackLED.High()
		} else {
			pinBlackLED.Low()
		}
		ledState.black = on
	case BinBrown:
		changed = ledState.brown != on
		name = "BROWN"
		if on {
			pinBrownLED.High()
		} else {
			pinBrownLED.Low()
		}
		ledState.brown = on
	}

	if changed && bindicatorLogger != nil {
		bindicatorLogger.Info("led:changed", slog.String("bin", name), slog.Bool("on", on))
	}
}

// updateLEDsFromSchedule checks the schedule and updates LED states.
// LED ON: 12 hours before collection (noon day before)
// LED OFF: 12 hours into collection day (noon on collection day)
func updateLEDsFromSchedule(jobs []BinJob, now time.Time) {
	// Skip LED updates during OTA
	if bindicatorPaused {
		return
	}

	greenOn := false
	blackOn := false
	brownOn := false

	if bindicatorLogger != nil {
		bindicatorLogger.Debug("schedule:checking",
			slog.Int("jobs", len(jobs)),
			slog.String("now", now.Format("2006-01-02 15:04")),
		)
	}

	for i := 0; i < len(jobs); i++ {
		job := &jobs[i]
		// Create collection date at midnight UTC
		collectionDate := time.Date(
			int(job.Year), time.Month(job.Month), int(job.Day),
			0, 0, 0, 0, time.UTC,
		)

		// Window start: noon the day before (12 hours before midnight of collection day)
		windowStart := collectionDate.Add(-12 * time.Hour)
		// Window end: noon on collection day (12 hours after midnight)
		windowEnd := collectionDate.Add(12 * time.Hour)

		// Check if current time is within the window
		inWindow := now.After(windowStart) && now.Before(windowEnd)

		if bindicatorLogger != nil {
			bindicatorLogger.Debug("schedule:job",
				slog.String("date", collectionDate.Format("2006-01-02")),
				slog.String("bin", job.Bin.String()),
			)
		}

		if inWindow {
			switch job.Bin {
			case BinGreen:
				greenOn = true
			case BinBlack:
				blackOn = true
			case BinBrown:
				brownOn = true
			}
		}
	}

	// Log next upcoming collection
	if bindicatorLogger != nil {
		for i := 0; i < len(jobs); i++ {
			job := &jobs[i]
			collectionDate := time.Date(
				int(job.Year), time.Month(job.Month), int(job.Day),
				0, 0, 0, 0, time.UTC,
			)
			// Find first collection that hasn't passed yet (noon on collection day)
			if now.Before(collectionDate.Add(12 * time.Hour)) {
				bindicatorLogger.Info("schedule:next",
					slog.String("date", collectionDate.Format("2006-01-02")),
					slog.String("bin", job.Bin.String()),
				)
				break
			}
		}
	}

	// Update LED states
	setLED(BinGreen, greenOn)
	setLED(BinBlack, blackOn)
	setLED(BinBrown, brownOn)
}

// getJobs returns the current job storage slice
func getJobs() []BinJob {
	return jobStorage[:jobCount]
}

// clearJobs resets the job count
func clearJobs() {
	jobCount = 0
}
