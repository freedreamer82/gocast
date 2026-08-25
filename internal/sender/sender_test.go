package sender

import (
	"strings"
	"testing"

	"gocast/internal/discovery"
	"gocast/internal/media"
)

func TestCropElement(t *testing.T) {
	// The buffer is the one GNOME hands over for a window: as large as the
	// monitor, with the content at the top left.
	const bufW, bufH = 3440, 1440

	t.Run("margins are measured from the buffer", func(t *testing.T) {
		got, err := cropElement("1280x800", bufW, bufH)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "right=2160") || !strings.Contains(got, "bottom=640") {
			t.Errorf("wrong margins in %q", got)
		}
	})

	t.Run("the origin is cropped from all four sides", func(t *testing.T) {
		// Cropping only right and bottom would keep the top-left margin inside
		// and shift the window in the transmitted image.
		got, err := cropElement("1280x800+40+20", bufW, bufH)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{"left=40", "top=20", "right=2120", "bottom=620"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %s in %q", want, got)
			}
		}
	})

	t.Run("a crop matching the buffer inserts nothing", func(t *testing.T) {
		if got, err := cropElement("3440x1440", bufW, bufH); err != nil || got != "" {
			t.Errorf("want no element, got %q (err %v)", got, err)
		}
	})

	t.Run("an offset origin must not overrun the buffer", func(t *testing.T) {
		if _, err := cropElement("3440x1440+10+10", bufW, bufH); err == nil {
			t.Error("want an error: origin plus size exceed the buffer")
		}
	})

	t.Run("without --crop nothing is inserted", func(t *testing.T) {
		if got, err := cropElement("", bufW, bufH); err != nil || got != "" {
			t.Errorf("want no element, got %q (err %v)", got, err)
		}
	})

	t.Run("a crop larger than the buffer is an error", func(t *testing.T) {
		// Better to stop here than hand videocrop negative margins, which would
		// fail negotiation with a far less clear message.
		if _, err := cropElement("4000x800", bufW, bufH); err == nil {
			t.Error("want an error for a crop beyond the buffer")
		}
	})

	t.Run("unrecognised format", func(t *testing.T) {
		for _, spec := range []string{"1280", "1280*800", "axb", "0x800"} {
			if _, err := cropElement(spec, bufW, bufH); err == nil {
				t.Errorf("want an error for %q", spec)
			}
		}
	})

	t.Run("without a buffer size there is nothing to crop against", func(t *testing.T) {
		if _, err := cropElement("1280x800", 0, 0); err == nil {
			t.Error("want an error when the portal declares no size")
		}
	})
}

func TestMuxLatency(t *testing.T) {
	// With two pads the muxer only produces when both have data: without a
	// tolerance it waits forever for the audio branch, which always arrives
	// later. Measured: 1 frame without it, 235 with 50 ms.
	if got := muxLatency("@DEFAULT_MONITOR@", 200); got != " latency=200000000" {
		t.Errorf("want the latency in nanoseconds, got %q", got)
	}

	t.Run("with video alone it is not needed", func(t *testing.T) {
		// A single pad waits for nobody: it would be latency added for nothing.
		if got := muxLatency("", 200); got != "" {
			t.Errorf("want no latency, got %q", got)
		}
	})

	t.Run("can be switched off", func(t *testing.T) {
		if got := muxLatency("@DEFAULT_MONITOR@", 0); got != "" {
			t.Errorf("want no latency, got %q", got)
		}
	})
}

func TestScaleChain(t *testing.T) {
	t.Run("fixed dimensions, never ranges", func(t *testing.T) {
		// With anything left to infer, videoscale on a PipeWire source runs into
		// a division by zero: the real caps are not known yet at fixation time.
		got := scaleChain(media.Rect{W: 1920, H: 804}, false)
		for _, want := range []string{"width=1920", "height=804", "pixel-aspect-ratio=1/1"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %s in %q", want, got)
			}
		}
		if strings.Contains(got, "[") {
			t.Errorf("no range may appear: %q", got)
		}
	})

	t.Run("nothing to shrink, no videoscale", func(t *testing.T) {
		got := scaleChain(media.Rect{}, false)
		if strings.Contains(got, "videoscale") {
			t.Errorf("videoscale should not appear, got %q", got)
		}
		if got != "! video/x-raw,format=I420" {
			t.Errorf("want the format alone, got %q", got)
		}
	})

	t.Run("the format is always I420", func(t *testing.T) {
		// Left to itself, videoconvert picks Y444 and x264enc encodes High 4:4:4,
		// which the Raspberry's hardware decoder does not handle.
		for _, r := range []media.Rect{{}, {W: 1280, H: 720}} {
			if !strings.Contains(scaleChain(r, false), "format=I420") {
				t.Errorf("format=I420 missing for %v", r)
			}
		}
	})
}

func TestEffectiveMaxWidth(t *testing.T) {
	// The receiver sets a ceiling; the sender may go below it, never above.
	cases := []struct{ declared, requested, want int }{
		{1920, 0, 1920},    // only the receiver declares one
		{1920, 1280, 1280}, // the sender wants to go lower: allowed
		{1920, 3440, 1920}, // the sender wants to go higher: held at the ceiling
		{1920, 1920, 1920},
		{0, 1280, 1280}, // the receiver sets no limit
		{0, 0, 0},       // no limit anywhere
	}
	for _, c := range cases {
		if got := effectiveMaxWidth(c.declared, c.requested); got != c.want {
			t.Errorf("effectiveMaxWidth(%d, %d) = %d, want %d",
				c.declared, c.requested, got, c.want)
		}
	}
}

func TestFitWithin(t *testing.T) {
	t.Run("shrinks keeping the proportions", func(t *testing.T) {
		got := fitWithin(media.Rect{W: 3440, H: 1440}, 1920)
		if want := (media.Rect{W: 1920, H: 804}); got != want {
			t.Errorf("want %v, got %v", want, got)
		}
	})

	t.Run("a source already under the ceiling is left alone", func(t *testing.T) {
		// An empty rectangle means "no videoscale": enlarging a small screen
		// makes no sense and would cost bandwidth.
		if got := fitWithin(media.Rect{W: 1280, H: 720}, 1920); !got.Empty() {
			t.Errorf("want no resizing, got %v", got)
		}
	})

	t.Run("even sides", func(t *testing.T) {
		// I420 does not allow odd sides: x264enc would refuse to initialise.
		got := fitWithin(media.Rect{W: 1000, H: 667}, 500)
		if got.W%2 != 0 || got.H%2 != 0 {
			t.Errorf("odd dimensions: %v", got)
		}
	})

	t.Run("no ceiling or no source means no resizing", func(t *testing.T) {
		if got := fitWithin(media.Rect{W: 3440, H: 1440}, 0); !got.Empty() {
			t.Errorf("without a ceiling: %v", got)
		}
		if got := fitWithin(media.Rect{}, 1920); !got.Empty() {
			t.Errorf("without a source: %v", got)
		}
	})
}

// With the receiver's frame the picture has to come out at exactly that size and
// with black bars: it is the only way kmssink will accept it, since it draws by
// setting a display mode. Without add-borders, videoscale would distort the
// picture to fill the frame.
func TestScaleChainFillsTheFrame(t *testing.T) {
	got := scaleChain(media.Rect{W: 1920, H: 1080}, true)
	for _, want := range []string{"add-borders=true", "width=1920", "height=1080"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// An ultrawide inside 16:9: bars above and below, the picture in proportion, and
// every number even because x264enc rejects odd dimensions.
func TestLetterboxUltrawideIn16by9(t *testing.T) {
	f := letterbox(media.Rect{W: 3440, H: 1440}, media.Rect{W: 1920, H: 1080})

	if f.inner.W != 1920 || f.inner.H != 804 {
		t.Errorf("inner picture %dx%d, want 1920x804", f.inner.W, f.inner.H)
	}
	if f.t != 138 || f.b != 138 {
		t.Errorf("vertical bars %d/%d, want 138/138", f.t, f.b)
	}
	if f.l != 0 || f.r != 0 {
		t.Errorf("horizontal bars %d/%d, want 0/0", f.l, f.r)
	}
	if f.inner.H+f.t+f.b != 1080 {
		t.Errorf("the total height does not add up: %d", f.inner.H+f.t+f.b)
	}
}

// A window taller than it is wide: the bars go to the sides.
func TestLetterboxNarrowWindow(t *testing.T) {
	f := letterbox(media.Rect{W: 600, H: 1000}, media.Rect{W: 1920, H: 1080})

	if f.inner.H != 1080 {
		t.Errorf("inner height %d, want 1080", f.inner.H)
	}
	if f.inner.W+f.l+f.r != 1920 {
		t.Errorf("the total width does not add up: %d", f.inner.W+f.l+f.r)
	}
	if f.t != 0 || f.b != 0 {
		t.Errorf("vertical bars %d/%d, want 0/0", f.t, f.b)
	}
}

// A source already of the right shape: no bars at all.
func TestLetterboxSameShape(t *testing.T) {
	f := letterbox(media.Rect{W: 1920, H: 1080}, media.Rect{W: 1920, H: 1080})
	if f.l|f.r|f.t|f.b != 0 {
		t.Errorf("unexpected bars: %d %d %d %d", f.l, f.r, f.t, f.b)
	}
	// Normalising the caps is needed anyway: it is what lets videoscale compute
	// the bars instead of giving up and distorting.
	if !strings.Contains(f.borderChain(), "pixel-aspect-ratio=1/1") {
		t.Errorf("caps not normalised: %q", f.borderChain())
	}
}

// To x264, key-int-max=0 means "one keyframe, at the very start": whoever joins
// later never receives one and video never appears, while audio plays. No error
// anywhere. Zero must never reach the pipeline.
func TestKeyintZeroBecomesOnePerSecond(t *testing.T) {
	if got := effectiveKeyint(0, 25); got != 25 {
		t.Errorf("keyint 0 with 25 fps = %d, want 25", got)
	}
	if got := effectiveKeyint(0, 0); got <= 0 {
		t.Errorf("with nothing sensible it returned %d", got)
	}
	if got := effectiveKeyint(10, 25); got != 10 {
		t.Errorf("an explicit choice was ignored: %d", got)
	}
}

// The cap on a single frame is what gets whole keyframes across a narrow link:
// without it they are all lost and video never appears.
func TestVbvCapsTheSingleFrame(t *testing.T) {
	got := vbvOptions(12000)
	for _, want := range []string{"vbv-maxrate=12000", "vbv-bufsize=600"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// At very low bitrates the buffer must not fall below a usable size, or the
	// encoder cannot stay within its limits and quality collapses without
	// anybody having asked for it.
	if !strings.Contains(vbvOptions(1000), "vbv-bufsize=200") {
		t.Errorf("minimum buffer not respected: %q", vbvOptions(1000))
	}
}

// A lower resolution is a request for a different display mode, not for an
// arbitrary smaller size: the receiver refuses anything that is not one of its
// screen's modes, so --width has to land on one of the announced ones.
func TestChooseFrame(t *testing.T) {
	r := discovery.Receiver{
		MaxWidth: 1920, MaxHeight: 1080,
		Modes: []media.Rect{
			{W: 1920, H: 1080}, {W: 1280, H: 720}, {W: 1024, H: 768}, {W: 800, H: 600},
		},
	}

	cases := []struct {
		name  string
		width int
		want  media.Rect
	}{
		{"without --width the preferred mode", 0, media.Rect{W: 1920, H: 1080}},
		{"exactly a mode", 1280, media.Rect{W: 1280, H: 720}},
		{"between two modes takes the lower one", 1200, media.Rect{W: 1024, H: 768}},
		{"above the screen stays at the preferred", 3440, media.Rect{W: 1920, H: 1080}},
		{"below every mode stays at the preferred", 320, media.Rect{W: 1920, H: 1080}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chooseFrame(r, c.width); got != c.want {
				t.Errorf("chooseFrame(%d) = %v, want %v", c.width, got, c.want)
			}
		})
	}

	t.Run("a receiver announcing no modes keeps the old behaviour", func(t *testing.T) {
		old := discovery.Receiver{MaxWidth: 1920, MaxHeight: 1080}
		if got := chooseFrame(old, 1280); got != (media.Rect{W: 1920, H: 1080}) {
			t.Errorf("want the preferred size, got %v", got)
		}
	})
}
