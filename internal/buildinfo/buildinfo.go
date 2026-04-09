// Package buildinfo provides shared build metadata for katamaran binaries.
package buildinfo

// Version is the release version, shared across all katamaran binaries to
// prevent drift during releases. Override at build time via ldflags:
//
//	go build -ldflags "-X github.com/maci0/katamaran/internal/buildinfo.Version=v1.0.0"
var Version = "v0.3.0"
