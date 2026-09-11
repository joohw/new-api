package buildinfo

import "strings"

// Set at link time via -ldflags (see .goreleaser.yaml).
var (
	Version = "dev0.2.61"
	Commit  = "none"
	Date    = "unknown"
)

// VersionString returns the release version without a leading "v".
func VersionString() string {
	return strings.TrimPrefix(strings.TrimSpace(Version), "v")
}

// Display returns a human-readable version line.
func Display() string {
	v := strings.TrimSpace(Version)
	if v == "" {
		v = "dev"
	}
	return v
}
