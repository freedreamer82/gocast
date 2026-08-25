package receiver

import (
	"context"
	"fmt"
	"gocast/internal/media"
	"log"
	"sync"
)

// pairScreen shows the pairing code on the receiver's own screen.
//
// This is the crux of the mechanism: the code does not travel over the network
// towards whoever asked for it, it appears on the display. Only somebody
// physically in front of the screen can read it and copy it down, and that is
// what makes pairing a proof of presence rather than a shared secret.
type pairScreen struct {
	sink string

	// The receiver's own context, not a fresh one: the screen has to die with the
	// program that lit it. Tied to context.Background() it survives shutdown, and
	// an orphaned gst-launch is left that nobody will ever stop.
	ctx context.Context

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (p *pairScreen) show(code string) {
	p.hide()

	base := p.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()

	// is-live=true, and the sink left to synchronise.
	//
	// videotestsrc is not a live source: in front of a sink with sync=false it
	// generates frames as fast as it can, ignoring the 10 per second it declares,
	// and a whole core goes into showing a caption that does not move. sync=false
	// belongs to live video, where frames arrive already paced by the network;
	// here it is exactly the opposite.
	desc := fmt.Sprintf(
		"-q videotestsrc is-live=true pattern=solid-color foreground-color=0xff10243f "+
			"! video/x-raw,width=1280,height=720,framerate=5/1 "+
			"! textoverlay text=\"Pairing code: %s\" "+
			"font-desc=\"Sans Bold 42\" valignment=center halignment=center "+
			"! videoconvert ! %s",
		code, p.sink)

	go func() {
		if err := media.RunPipeline(ctx, desc); err != nil && ctx.Err() == nil {
			log.Printf("pairing screen not shown: %v", err)
		}
	}()
}

func (p *pairScreen) hide() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}
