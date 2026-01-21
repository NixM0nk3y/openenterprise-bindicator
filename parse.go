package main

// parsedTimestamp holds the Unix timestamp from the last parsed response
var parsedTimestamp int64

// parseScheduleResponse parses the CSV format from Node-RED.
// Format: "TIMESTAMP,YYYY-MM-DD:TYPE,YYYY-MM-DD:TYPE,..."
// Example: "1737207000,2026-01-17:BLACK,2026-01-31:GREEN"
// Returns the number of jobs parsed into jobStorage.
// The Unix timestamp is stored in parsedTimestamp.
func parseScheduleResponse(data []byte) int {
	clearJobs()
	parsedTimestamp = 0

	if len(data) == 0 {
		return 0
	}

	// Parse Unix timestamp from the beginning
	pos := 0
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		parsedTimestamp = parsedTimestamp*10 + int64(data[pos]-'0')
		pos++
	}

	// Skip the comma after timestamp
	if pos < len(data) && data[pos] == ',' {
		pos++
	}

	for pos < len(data) && jobCount < maxJobs {
		// Find end of current entry (comma or end of data)
		entryEnd := pos
		for entryEnd < len(data) && data[entryEnd] != ',' {
			entryEnd++
		}

		// Parse entry: YYYY-MM-DD:TYPE
		entry := data[pos:entryEnd]
		if len(entry) >= 11 { // Minimum: "YYYY-MM-DD:X"
			// Find colon separator
			colonIdx := -1
			for i := 0; i < len(entry); i++ {
				if entry[i] == ':' {
					colonIdx = i
					break
				}
			}

			if colonIdx == 10 { // Date is exactly 10 chars
				// Parse date: YYYY-MM-DD
				year := atoi4(entry[0:4])
				month := atoi2(entry[5:7])
				day := atoi2(entry[8:10])

				// Parse bin type
				binType := entry[colonIdx+1:]
				bt := parseBinType(binType)

				if bt != BinUnknown && year > 0 && month > 0 && month <= 12 && day > 0 && day <= 31 {
					jobStorage[jobCount].Year = uint16(year)
					jobStorage[jobCount].Month = uint8(month)
					jobStorage[jobCount].Day = uint8(day)
					jobStorage[jobCount].Bin = bt
					jobCount++
				}
			}
		}

		// Move to next entry
		pos = entryEnd + 1
	}

	return jobCount
}

// atoi2 converts 2-digit ASCII string to int without allocation
func atoi2(s []byte) int {
	if len(s) < 2 {
		return 0
	}
	d1 := int(s[0] - '0')
	d2 := int(s[1] - '0')
	if d1 < 0 || d1 > 9 || d2 < 0 || d2 > 9 {
		return 0
	}
	return d1*10 + d2
}

// atoi4 converts 4-digit ASCII string to int without allocation
func atoi4(s []byte) int {
	if len(s) < 4 {
		return 0
	}
	d1 := int(s[0] - '0')
	d2 := int(s[1] - '0')
	d3 := int(s[2] - '0')
	d4 := int(s[3] - '0')
	if d1 < 0 || d1 > 9 || d2 < 0 || d2 > 9 || d3 < 0 || d3 > 9 || d4 < 0 || d4 > 9 {
		return 0
	}
	return d1*1000 + d2*100 + d3*10 + d4
}

// parseBinType converts bin type string to BinType enum
func parseBinType(s []byte) BinType {
	if len(s) == 0 {
		return BinUnknown
	}

	// Match based on first character and length
	switch s[0] {
	case 'G', 'g': // GREEN
		if len(s) >= 5 {
			return BinGreen
		}
	case 'B', 'b':
		if len(s) >= 5 {
			// Distinguish BLACK from BROWN by second char
			if len(s) >= 2 && (s[1] == 'L' || s[1] == 'l') {
				return BinBlack
			}
			if len(s) >= 2 && (s[1] == 'R' || s[1] == 'r') {
				return BinBrown
			}
		}
	}
	return BinUnknown
}
