package media

import "testing"

// frameAt builds a BGRA buffer with an opaque rectangle at the given origin,
// the way the compositor paints a window inside a buffer as large as the
// monitor.
func frameAt(bufW, bufH, x, y, w, h int) []byte {
	b := make([]byte, bufW*bufH*4)
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			px := (row*bufW + col) * 4
			b[px+0] = 0x20 // blue
			b[px+1] = 0x30 // green
			b[px+2] = 0x40 // red
			b[px+3] = 0xff // alpha
		}
	}
	return b
}

func frame(bufW, bufH, w, h int) []byte { return frameAt(bufW, bufH, 0, 0, w, h) }

func TestEvenDown(t *testing.T) {
	// A window 529 pixels tall is entirely ordinary, and x264enc rejects it
	// without explaining why: I420 does not allow odd sides.
	cases := map[int]int{529: 528, 702: 702, 1: 0, 0: 0, 1081: 1080}
	for in, want := range cases {
		if got := EvenDown(in); got != want {
			t.Errorf("EvenDown(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBoundingBox(t *testing.T) {
	const bufW, bufH = 64, 32

	t.Run("measures the opaque rectangle at the origin", func(t *testing.T) {
		got := BoundingBox(frame(bufW, bufH, 40, 20), bufW, bufH, OpaqueAbove(255))
		if want := (Rect{W: 40, H: 20}); got != want {
			t.Errorf("want %v, got %v", want, got)
		}
	})

	t.Run("measures the origin too when the content is offset", func(t *testing.T) {
		// Measuring only the extent would keep the top-left margin inside, and
		// the window would end up out of place in the transmitted image.
		got := BoundingBox(frameAt(bufW, bufH, 6, 4, 40, 20), bufW, bufH, OpaqueAbove(255))
		if want := (Rect{X: 6, Y: 4, W: 40, H: 20}); got != want {
			t.Errorf("want %v, got %v", want, got)
		}
	})

	t.Run("content filling the buffer", func(t *testing.T) {
		got := BoundingBox(frame(bufW, bufH, bufW, bufH), bufW, bufH, OpaqueAbove(255))
		if want := (Rect{W: bufW, H: bufH}); got != want {
			t.Errorf("want the whole buffer, got %v", got)
		}
	})

	t.Run("fully transparent buffer", func(t *testing.T) {
		if got := BoundingBox(make([]byte, bufW*bufH*4), bufW, bufH, OpaqueAbove(255)); !got.Empty() {
			t.Errorf("want no content, got %v", got)
		}
	})

	t.Run("the window shadow stays out of the crop", func(t *testing.T) {
		// GNOME paints the shadow into the buffer as well, with partial alpha and
		// wider than the window. Measured on a real case: 702x529 with threshold
		// 1, 680x504 with threshold 255.
		b := frameAt(bufW, bufH, 6, 4, 40, 20)
		for y := 2; y < 28; y++ {
			for x := 2; x < 52; x++ {
				px := (y*bufW + x) * 4
				if b[px+3] == 0 {
					b[px+3] = 0x30 // penumbra
				}
			}
		}
		if got := BoundingBox(b, bufW, bufH, OpaqueAbove(255)); got != (Rect{X: 6, Y: 4, W: 40, H: 20}) {
			t.Errorf("the shadow got into the crop: %v", got)
		}
		if got := BoundingBox(b, bufW, bufH, OpaqueAbove(1)); got.W != 50 {
			t.Errorf("without a threshold the shadow should get in, got %v", got)
		}
	})

	t.Run("falls back to non-black pixels when alpha is opaque everywhere", func(t *testing.T) {
		b := frame(bufW, bufH, 40, 20)
		for i := 3; i < len(b); i += 4 {
			b[i] = 0xff
		}
		if got := BoundingBox(b, bufW, bufH, OpaqueAbove(255)); got != (Rect{W: bufW, H: bufH}) {
			t.Errorf("with alpha opaque everywhere the alpha rule takes it all: %v", got)
		}
		if got := BoundingBox(b, bufW, bufH, NotBlack); got != (Rect{W: 40, H: 20}) {
			t.Errorf("want 40x20 from the fallback, got %v", got)
		}
	})
}

func TestRectEven(t *testing.T) {
	// The corner stays put: a row or a column is given up instead.
	got := Rect{X: 7, Y: 3, W: 681, H: 505}.Even()
	if want := (Rect{X: 7, Y: 3, W: 680, H: 504}); got != want {
		t.Errorf("want %v, got %v", want, got)
	}
}
