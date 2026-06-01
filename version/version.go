// Package version exposes the build stamp, set via -ldflags at build time.
package version

// Set via ldflags at build time.
var (
	Commit    string
	BuildDate string
)
