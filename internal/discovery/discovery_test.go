package discovery

import (
	"strings"
	"testing"

	"gocast/internal/control"
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

// Whether we have paired is not something the receiver can tell us: it
// announces that it wants a code, and only this machine knows whether it holds
// one. Nothing filled the answer in, so a receiver already paired went on being
// reported as needing pairing — the extension kept offering "pair first" and
// the sender refused to transmit to it at all.
func TestPairedIsAnsweredHere(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	r := Receiver{Name: "tv", Host: "192.168.1.9", Port: 5000,
		ID: "8e36a12f01f68890", Pairing: true}
	if r.paired() {
		t.Error("with no code stored, a receiver cannot be reported as paired")
	}
	if got := pairingState(r); got != "pairing needed (gocast pair)" {
		t.Errorf("unpaired receiver reported as %q", got)
	}

	if err := control.RememberPin(r.Key(), "2040"); err != nil {
		t.Fatalf("the code could not be stored: %v", err)
	}

	// Read back out of an announcement, the way the extension and the sender get
	// it: the answer has to be filled in there, not left to each caller.
	r = receiverFrom("tv", "192.168.1.9", 5000,
		[]string{"pairing=1", "id=8e36a12f01f68890", "maxw=1920", "maxh=1080"})
	if !r.Paired {
		t.Fatal("an announcement from a receiver whose code we hold must come back paired")
	}
	if !r.Pairing || r.MaxWidth != 1920 {
		t.Errorf("the rest of the announcement was lost: %+v", r)
	}
	if got := pairingState(r); got != "paired" {
		t.Errorf("paired receiver reported as %q", got)
	}

	// The code is filed under the announced identity and not the address, so a
	// receiver that moves under DHCP must not have to be paired again.
	moved := r
	moved.Host = "192.168.1.44"
	if !moved.paired() {
		t.Error("pairing must survive a change of address")
	}
}
