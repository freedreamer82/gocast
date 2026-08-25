// Package version carries the build identity, so that both ends can say what
// they are running.
//
// It exists because the two halves are installed separately, on machines that
// are updated at different times: the receiver is a service on a Raspberry, the
// sender a binary on a PC, and the GNOME extension launches whatever is on the
// path. A mismatch between them does not announce itself — it shows up as a
// feature that "does not work", and the first hour goes into the wrong place.
package version

import "runtime/debug"

// Number is the release, bumped by hand.
const Number = "0.2.0"

// Commit is filled in at build time by the Makefile:
//
//	-ldflags "-X gocast/internal/version.Commit=$(git rev-parse --short HEAD)"
//
// Empty in a plain `go build`, which is fine: the release number alone still
// tells the two ends apart.
var Commit string

// String is what gets announced, logged and shown.
func String() string {
	if Commit != "" {
		return Number + "+" + Commit
	}
	if c := vcsCommit(); c != "" {
		return Number + "+" + c
	}
	return Number
}

// vcsCommit reads the revision Go stamps into the binary when building inside a
// repository, so that a plain `go build` is identified too.
func vcsCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return ""
}
