package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// A handful of frames, not one: the first may arrive before the window has been
// composited into the buffer, and would contain nothing but transparency.
const framesProbed = 5

// DetectContent works out the rectangle the useful content occupies inside the
// buffer the portal hands over.
//
// When a single window is shared, GNOME paints it into a buffer as large as the
// monitor and leaves the rest untouched. The geometry is not declared anywhere —
// there is no position among the stream properties, and the caps only carry the
// buffer size — but it can be read off the pixels: the format is BGRA and the
// area outside the window has zero alpha.
//
// So we grab one frame with a throwaway pipeline and look for the rectangle of
// opaque pixels. If alpha is opaque everywhere, as on a full screen, we fall
// back to non-black pixels; if those fill the buffer too, there is nothing to
// crop.
func DetectContent(ctx context.Context, src string, bufW, bufH, alphaMin int, extra ...*os.File) (Rect, error) {
	last, err := CaptureFrame(ctx, src, bufW, bufH, extra...)
	if err != nil {
		return Rect{}, err
	}

	full := Rect{W: bufW, H: bufH}
	for _, keep := range []func([]byte) bool{OpaqueAbove(alphaMin), NotBlack} {
		r := BoundingBox(last, bufW, bufH, keep)
		if r.Empty() {
			continue
		}
		if r == full {
			return full.Even(), nil // fills the buffer: nothing to crop
		}
		return r.Even(), nil
	}
	return full.Even(), nil
}

// How long to wait for a frame from the source before giving up.
const frameWait = 4 * time.Second

// CaptureFrame grabs one raw BGRA frame from the source.
func CaptureFrame(ctx context.Context, src string, bufW, bufH int, extra ...*os.File) ([]byte, error) {
	if bufW <= 0 || bufH <= 0 {
		return nil, errors.New("the portal did not declare a buffer size")
	}

	f, err := os.CreateTemp("", "gocast-frame-*.raw")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	desc := fmt.Sprintf("-q %s num-buffers=%d ! video/x-raw,format=BGRA ! filesink location=%s",
		src, framesProbed, name)

	// With a deadline, and not out of generic caution.
	//
	// pipewiresrc delivers a frame only when the screen changes: share a window
	// that is standing still — an editor, a document, anything nobody is working
	// on right now — and the frame never arrives, leaving this measurement hung
	// forever. The sender then transmits nothing and says nothing: it looks like
	// it started, and not one packet goes out.
	//
	// Once the time is up we give up on cropping and send the whole buffer,
	// which is exactly what is wanted: better uncropped than stuck.
	probeCtx, cancel := context.WithTimeout(ctx, frameWait)
	defer cancel()

	if err := RunPipeline(probeCtx, desc, extra...); err != nil {
		if probeCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("no frame within %s: the source is not changing", frameWait)
		}
		return nil, fmt.Errorf("grabbing a frame: %w", err)
	}

	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}

	frame := bufW * bufH * 4
	if frame <= 0 || len(data) < frame {
		return nil, fmt.Errorf("incomplete frame: %d bytes for a %dx%d buffer",
			len(data), bufW, bufH)
	}
	// The last whole frame: the first ones may still be empty.
	return data[(len(data)/frame-1)*frame:][:frame], nil
}

// EvenDown rounds down to an even number.
//
// I420 subsamples chroma by a factor of two and does not tolerate odd sides:
// x264enc refuses to initialise with a message that does not explain why ("Can
// not initialize x264 encoder"). Detection returns the window's real size,
// which is as likely to be odd as even, so we give up a row or a column.
func EvenDown(n int) int { return n &^ 1 }

// Pixels are BGRA: blue, green, red, alpha.

// OpaqueAbove selects the pixels opaque enough to count as window.
//
// The default is 255 — only fully opaque pixels — and it is not a threshold
// picked by eye: the compositor paints the inside of the window fully opaque
// and reserves the single maximum value for it, while the shadow around it is
// partially transparent by construction. Telling the two apart therefore
// requires estimating nothing: asking for the maximum is enough.
//
// It stays adjustable only as an escape hatch, for a source that painted its
// window slightly translucent.
func OpaqueAbove(min int) func([]byte) bool {
	return func(px []byte) bool { return int(px[3]) >= min }
}

func NotBlack(px []byte) bool { return px[0] != 0 || px[1] != 0 || px[2] != 0 }

// Rect is the rectangle the content occupies inside the buffer.
type Rect struct{ X, Y, W, H int }

func (r Rect) String() string { return fmt.Sprintf("%dx%d+%d+%d", r.W, r.H, r.X, r.Y) }

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Even shrinks the rectangle to even sides while keeping its corner in place:
// I420 subsamples chroma by a factor of two and does not allow odd sides, and
// x264enc rejects them with a message that does not explain why.
func (r Rect) Even() Rect {
	r.W = EvenDown(r.W)
	r.H = EvenDown(r.H)
	return r
}

// BoundingBox measures the rectangle the content occupies, **origin included**.
//
// Measuring only the extent is not enough: assuming the compositor anchors the
// window at the top left, content that is even slightly offset gets cropped
// with that margin kept inside, and in the transmitted image the window ends up
// out of place.
func BoundingBox(frame []byte, w, h int, keep func([]byte) bool) Rect {
	minX, minY := w, h
	maxX, maxY := -1, -1

	for y := 0; y < h; y++ {
		row := frame[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			if !keep(row[x*4 : x*4+4]) {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			maxY = y
		}
	}

	if maxX < 0 {
		return Rect{} // the rule selects nothing
	}
	return Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1}
}
