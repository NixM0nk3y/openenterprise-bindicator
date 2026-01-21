package version

// Build information (injected via ldflags - must NOT have default values)
var (
	Version   string
	GitSHA    string
	BuildDate string
)

// Hardcoded build marker - change this to verify correct firmware is flashed
const BuildMarker = "build-022"
