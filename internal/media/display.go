package media

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DisplayMode reads the resolution of the attached screen.
//
// It matters because kmssink is not an ordinary sink: it draws by setting a
// display mode, and the modes are the ones the monitor advertises — 1920x1080,
// 1280x720. A frame of any other size is not scaled down or letterboxed, it is
// refused during negotiation, and the stream dies with a "not-negotiated" error
// that surfaces all the way back at the source. Measured: 1920x804 refused,
// 1920x1080 drawn.
//
// The value is read from the kernel rather than guessed: /sys/class/drm lists
// the connectors, and for the connected ones the modes file has the preferred
// mode on its first line.
func DisplayMode() Rect {
	paths, _ := filepath.Glob("/sys/class/drm/card*-*/modes")
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// A disconnected connector has an empty file: move on to the next one.
		for _, line := range strings.Split(string(data), "\n") {
			if r := parseMode(strings.TrimSpace(line)); !r.Empty() {
				return r
			}
		}
	}
	return Rect{}
}

// DisplayModes lists every distinct mode the attached screen advertises, the
// preferred one first.
//
// The receiver announces these so that a sender asking for a lower resolution
// can pick one the display will actually accept. kmssink draws by setting a
// mode, so an arbitrary smaller size is refused: 1280x720 works because the
// screen offers it, 1200x675 does not because no screen does.
func DisplayModes() []Rect {
	paths, _ := filepath.Glob("/sys/class/drm/card*-*/modes")
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		var out []Rect
		seen := map[Rect]bool{}
		for _, line := range strings.Split(string(data), "\n") {
			r := parseMode(strings.TrimSpace(line))
			// Interlaced modes parse to the same size as their progressive twin,
			// which is why duplicates are dropped rather than counted.
			if r.Empty() || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// parseMode reads a line such as "1920x1080".
func parseMode(s string) Rect {
	w, h, ok := strings.Cut(s, "x")
	if !ok {
		return Rect{}
	}
	// Some kernels append the refresh rate to the height: "1920x1080i60". Keep
	// the leading digits and discard the rest.
	h = leadingDigits(h)
	W, err1 := strconv.Atoi(w)
	H, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || W <= 0 || H <= 0 {
		return Rect{}
	}
	return Rect{W: W, H: H}
}

func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}
