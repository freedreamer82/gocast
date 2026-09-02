package receiver

import (
	"context"
	"fmt"
	"gocast/internal/media"
	"log"
	"sync"
	"time"
)

// pairScreen shows the pairing code on the receiver's own screen.
//
// This is the crux of the mechanism: the code does not travel over the network
// towards whoever asked for it, it appears on the display. Only somebody
// physically in front of the screen can read it and copy it down, and that is
// what makes pairing a proof of presence rather than a shared secret.
type pairScreen struct {
	sink  string
	frame media.Rect
	image string // the idle picture, if there is one: the code is written over it

	// The receiver's own context, not a fresh one: the screen has to die with the
	// program that lit it. Tied to context.Background() it survives shutdown, and
	// an orphaned gst-launch is left that nobody will ever stop.
	ctx context.Context

	// idle is the screen the code has to displace. Both drive the same sink, and
	// the sink owns the display: with the idle pipeline still holding it the code
	// sets no mode and draws nothing, so the receiver goes on showing its splash
	// while the sender waits for a code nobody can read.
	idle *idleScreen

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *pairScreen) show(code string) {
	// The idle screen is up whenever nobody is transmitting, which is exactly
	// when a code can be asked for — pairing is refused during a transmission.
	// So the normal way the code appears is as a caption written over the picture
	// already on the television: nothing is torn down, nothing is relit, and the
	// screen does not blink.
	//
	// The pipeline below is what is left for the case the caption has nowhere to
	// go: the instant between a stream ending and the idle screen coming back.
	if p.idle.caption("Pairing code: " + code) {
		return
	}

	p.hide()
	p.idle.hide() // waits for the process to be gone: the display must be free

	base := p.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.mu.Unlock()

	// The frame is the screen's own mode, not a round number: kmssink sets a
	// display mode and takes only the exact size of one, so a hardcoded 720p is
	// refused by a monitor that has no 720p mode among its own.
	w, h := p.frame.W, p.frame.H
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}

	// The caption is written over the idle picture rather than in place of it: on
	// a receiver given a background image, replacing it with a blue panel for the
	// length of the pairing window looks like the receiver has fallen over.
	// shaded-background because the text has to stay readable over a picture
	// whose colours nobody chose with a caption in mind.
	//
	// sync=true and one frame a second, for the reason spelled out in idle.go: a
	// still picture in front of a sink that does not synchronise is redrawn as
	// fast as the source can manage, and a whole core goes into showing something
	// that never changes.
	desc := fmt.Sprintf(
		"-q filesrc location=%q ! decodebin ! imagefreeze "+
			"! videoconvert ! videoscale add-borders=true "+
			"! video/x-raw,format=I420,width=%d,height=%d,framerate=1/1 "+
			"! textoverlay text=\"Pairing code: %s\" "+
			"font-desc=\"Sans Bold 42\" shaded-background=true "+
			"valignment=bottom halignment=center ypad=%d "+
			"! videoconvert ! %s sync=true",
		p.image, w, h, code, h/10, p.sink)

	// Without a picture the code goes on the same panel the idle splash uses, so
	// the two screens still look like the same program.
	if p.image == "" {
		desc = fmt.Sprintf(
			"-q videotestsrc is-live=true pattern=solid-color foreground-color=0xff10243f "+
				"! video/x-raw,width=%d,height=%d,framerate=1/1 "+
				"! textoverlay text=\"Pairing code: %s\" "+
				"font-desc=\"Sans Bold 42\" valignment=center halignment=center "+
				"! videoconvert ! %s sync=true",
			w, h, code, p.sink)
	}

	go func() {
		defer close(done)
		if err := media.RunPipeline(ctx, desc); err != nil && ctx.Err() == nil {
			log.Printf("pairing screen not shown: %v", err)
		}
	}()
}

// hide takes the code down and waits for the process to be gone, for the same
// reason the idle screen does: whatever lights the display next — the idle
// splash, or the playback pipeline — cannot set a mode until this one has let
// the DRM device go.
func (p *pairScreen) hide() {
	if p == nil {
		return
	}
	// Whichever way the code went up it has to come down, and clearing the
	// caption also forgets it: otherwise a spent code would reappear with the
	// idle screen after the next transmission.
	p.idle.caption("")

	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Print("the pairing screen is taking its time to close")
	}
}
