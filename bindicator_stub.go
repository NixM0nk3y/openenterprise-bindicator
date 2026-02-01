//go:build !tinygo

package main

// This file provides stub definitions for the regular Go toolchain (staticcheck, go vet).
// The actual implementation is in bindicator.go (TinyGo only).

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
