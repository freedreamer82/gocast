package receiver

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"gocast/internal/media"
)

// idleScreen is what the TV shows while nobody is transmitting.
//
// Without it the display falls back to whatever the console is showing — a
// login prompt, or the last kernel messages — which is both ugly and confusing:
// somebody looking at the TV cannot tell a receiver that is waiting from one
// that has crashed.
//
// It is the same mechanism as the pairing screen: a throwaway pipeline holding
// the sink, torn down the moment something more important needs the display.
type idleScreen struct {
	sink  string
	frame media.Rect
	image string // path to a picture; empty means the generated splash
	name  string // receiver name, shown on the splash

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// desc builds the pipeline that paints the idle screen.
//
// sync=true, and that is deliberate. A still picture in front of a sink with
// sync=false is redrawn as fast as the source can produce it, which costs a
// whole core to show something that never changes — the mistake already paid
// for once on the pairing screen. Paced by the clock at one frame a second it
// costs nothing.
func (s *idleScreen) desc() string {
	w, h := s.frame.W, s.frame.H
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}

	if s.image != "" {
		// add-borders because a picture rarely has the screen's proportions, and
		// kmssink accepts only the exact size of a display mode.
		return fmt.Sprintf(
			"-q filesrc location=%q ! decodebin ! imagefreeze "+
				"! videoconvert ! videoscale add-borders=true "+
				"! video/x-raw,format=I420,width=%d,height=%d,framerate=1/1 "+
				"! %s sync=true",
			s.image, w, h, s.sink)
	}

	return fmt.Sprintf(
		"-q videotestsrc is-live=true pattern=solid-color foreground-color=0xff10243f "+
			"! video/x-raw,width=%d,height=%d,framerate=1/1 "+
			"! textoverlay text=%q font-desc=\"Sans Bold 40\" "+
			"valignment=center halignment=center "+
			"! videoconvert ! %s sync=true",
		w, h, "gocast · "+s.name+" · ready", s.sink)
}

// show paints the idle screen, replacing whatever it was showing before.
func (s *idleScreen) show(ctx context.Context) {
	if s == nil {
		return
	}
	s.hide()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	s.mu.Lock()
	s.cancel, s.done = cancel, done
	s.mu.Unlock()

	desc := s.desc()
	go func() {
		defer close(done)
		if err := media.RunPipeline(runCtx, desc); err != nil && runCtx.Err() == nil {
			log.Printf("idle screen not shown: %v", err)
		}
	}()
}

// hide takes the idle screen down and waits for the process to be gone.
//
// Waiting is the point: the sink holds the DRM device, and starting playback
// while the idle pipeline still owns it means the video pipeline cannot set a
// mode — it exits successfully and draws nothing, which is the hardest kind of
// failure to read.
func (s *idleScreen) hide() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Print("the idle screen is taking its time to close")
	}
}

// checkIdleImage reports whether the picture can actually be shown, at startup
// rather than at the first use.
//
// A missing file or a missing decoder would otherwise surface as a black screen
// hours later, with nothing to connect it to the option that caused it.
func checkIdleImage(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("idle image: %w", err)
	}
	if !media.HasElement("imagefreeze") || !media.HasElement("decodebin") {
		return fmt.Errorf("showing an idle image needs the imagefreeze and decodebin " +
			"elements: sudo apt install gstreamer1.0-plugins-good")
	}
	return nil
}
