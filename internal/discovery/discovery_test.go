package discovery

import (
	"strings"
	"testing"

	"gocast/internal/media"
)

func modes(pairs ...[2]int) []media.Rect {
	var out []media.Rect
	for _, p := range pairs {
		out = append(out, media.Rect{W: p[0], H: p[1]})
	}
	return out
}

// A TXT record has room for a handful of modes, and the sender picks the
// largest one at or below the requested width. Sizes of a different shape than
// the screen's own would push the useful ones out of that handful, and a 16:9
// TV would end up offering its 5:4 leftovers.
func TestEncodeModesKeepsOnlyTheScreenShape(t *testing.T) {
	got := encodeModes(modes(
		[2]int{1920, 1080},
		[2]int{1280, 1024}, // 5:4
		[2]int{1280, 720},
		[2]int{1280, 800}, // 16:10
		[2]int{720, 400},
	))
	want := "1920x1080,1280x720,720x400"
	if got != want {
		t.Fatalf("encodeModes = %q, want %q", got, want)
	}
}

func TestEncodeModesSortsWidestFirst(t *testing.T) {
	got := encodeModes(modes([2]int{1280, 720}, [2]int{1920, 1080}, [2]int{1600, 900}))
	if want := "1920x1080,1600x900,1280x720"; got != want {
		t.Fatalf("encodeModes = %q, want %q", got, want)
	}
}

func TestEncodeModesStopsAtTheAnnouncementLimit(t *testing.T) {
	var in []media.Rect
	for w := 1920; w > 0; w -= 80 { // 24 modes, all 16:9
		in = append(in, media.Rect{W: w, H: w * 9 / 16})
	}
	got := encodeModes(in)
	if n := len(strings.Split(got, ",")); n != maxAnnouncedModes {
		t.Fatalf("announced %d modes, want %d: %q", n, maxAnnouncedModes, got)
	}
	if !strings.HasPrefix(got, "1920x1080,") {
		t.Fatalf("the widest mode must survive the cut: %q", got)
	}
}

// An empty entry would be announced as "0x0" and read back as a mode the sender
// could try to use.
func TestEncodeModesDropsEmpty(t *testing.T) {
	if got := encodeModes(modes([2]int{1920, 1080}, [2]int{0, 0})); got != "1920x1080" {
		t.Fatalf("encodeModes = %q", got)
	}
	if got := encodeModes(nil); got != "" {
		t.Fatalf("encodeModes(nil) = %q, want empty", got)
	}
}
