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

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	text    string             // the caption currently written over the picture
	feed    *os.File           // write end of the pipe the caption travels down
	ctx     context.Context    // the receiver's context: the chain dies with it
	started time.Time          // when the pipeline was lit: cues are timed from it
	hush    context.CancelFunc // stops the chain of cues currently being written
}

// How the caption is kept on the picture.
//
// One cue and no more would show it for a single frame: a text buffer whose
// duration GStreamer cannot work out is dropped as soon as the next frame
// arrives, and the code flashed and vanished — measured, frame by frame, not
// guessed. A cue carries its own start and end, so instead the chain is topped
// up: every cue is written before the one in front of it runs out.
//
// The lead is not politeness either: subparse holds a cue back until it has seen
// the one after it, so a chain kept only just ahead of the picture shows a gap
// at every boundary.
const (
	cueSpan = 1 * time.Second        // how long one cue lasts
	cueLead = 2 * time.Second        // how far ahead of the picture the chain is kept
	cueTick = 500 * time.Millisecond // how often it is topped up
)

// desc builds the pipeline that paints the idle screen.
//
// sync=true, and that is deliberate. A still picture in front of a sink with
// sync=false is redrawn as fast as the source can produce it, which costs a
// whole core to show something that never changes — the mistake already paid
// for once on the pairing screen. Paced by the clock at one frame a second it
// costs nothing.
//
// captioned adds an overlay whose words arrive down a pipe while the pipeline
// runs, on file descriptor 3. It is what lets the pairing code appear over the
// picture without the screen being taken down and put back up: the television
// would blink, change mode, and show the console in between — for a caption.
//
// wait-text=false or the picture would stop until something is said, which is
// most of the time. subparse and not raw text: what travels down the pipe is
// subtitle cues, which carry the times the overlay needs to keep the words up
// for longer than a single frame.
func (s *idleScreen) desc(captioned bool) string {
	w, h := s.frame.W, s.frame.H
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}

	caption, feed := "", ""
	if captioned {
		caption = fmt.Sprintf("textoverlay name=cap wait-text=false "+
			"font-desc=\"Sans Bold 42\" shaded-background=true "+
			"valignment=bottom halignment=center ypad=%d ! ", h/10)
		feed = " fdsrc fd=3 ! subparse ! cap.text_sink"
	}

	if s.image != "" {
		// add-borders because a picture rarely has the screen's proportions, and
		// kmssink accepts only the exact size of a display mode.
		return fmt.Sprintf(
			"-q filesrc location=%q ! decodebin ! imagefreeze "+
				"! videoconvert ! videoscale add-borders=true "+
				"! video/x-raw,format=I420,width=%d,height=%d,framerate=1/1 "+
				"! %s%s sync=true%s",
			s.image, w, h, caption, s.sink, feed)
	}

	// Two overlays on the splash, not one: the name is a fixed property of the
	// pipeline, the caption arrives down the pipe, and an element fed through its
	// text pad ignores the text it was given as a property.
	return fmt.Sprintf(
		"-q videotestsrc is-live=true pattern=solid-color foreground-color=0xff10243f "+
			"! video/x-raw,width=%d,height=%d,framerate=1/1 "+
			"! textoverlay text=%q font-desc=\"Sans Bold 40\" "+
			"valignment=center halignment=center "+
			"! %svideoconvert ! %s sync=true%s",
		w, h, "gocast · "+s.name+" · ready", caption, s.sink, feed)
}

// caption writes a line over the picture, leaving the picture alone.
//
// This is what the pairing code uses: the pipeline is not restarted, only the
// words on top of it change, and the screen never goes dark. The empty string
// clears it, within a second or so — what is already in flight has to run out
// first.
//
// Reports whether there was a screen to write on: with none, the caller has to
// light one of its own.
func (s *idleScreen) caption(text string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	// Remembered even when it cannot be delivered: show() writes it as soon as
	// there is a pipeline to write to, and clearing it here is what stops a
	// spent code from coming back with the screen.
	s.text = text
	feed, started, base, hush := s.feed, s.started, s.ctx, s.hush
	s.hush = nil
	s.mu.Unlock()

	// Whatever was being said stops being said: no more cues are written, and
	// the picture is clear again once the last one already in flight runs out.
	if hush != nil {
		hush()
	}
	if feed == nil {
		return false
	}
	if text == "" {
		return true
	}

	ctx, cancel := context.WithCancel(base)
	s.mu.Lock()
	s.hush = cancel
	s.mu.Unlock()
	go s.say(ctx, feed, started, text)
	return true
}

// say keeps the chain of cues ahead of the picture until it is told to stop.
func (s *idleScreen) say(ctx context.Context, feed *os.File, started time.Time, text string) {
	horizon := time.Since(started)
	for n := 0; ; {
		for horizon < time.Since(started)+cueLead {
			n++
			// A deadline because this is a pipe into another process: were nobody
			// reading it, this goroutine would sit here for the life of the program.
			_ = feed.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := feed.WriteString(cue(n, horizon, text)); err != nil {
				return // the screen has gone: there is nothing to write on
			}
			horizon += cueSpan
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cueTick):
		}
	}
}

// cue is one subtitle in the format subparse reads.
func cue(n int, start time.Duration, text string) string {
	return fmt.Sprintf("%d\n%s --> %s\n%s\n\n",
		n, cueTime(start), cueTime(start+cueSpan), text)
}

func cueTime(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d",
		int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, d.Milliseconds()%1000)
}

// show paints the idle screen.
//
// Called on a screen that is already up it does nothing at all, and that is the
// point: the caller asking for the idle screen back after a pairing code does
// not know the code was only a caption over it, and restarting the pipeline to
// paint the same picture would blink the television for nothing.
func (s *idleScreen) show(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	up := s.cancel != nil
	s.mu.Unlock()
	if up {
		return
	}

	// The caption channel, and only where there is something to parse the cues:
	// without subparse the screen still works, it just cannot be written on, and
	// the pairing code falls back to a screen of its own. A downgrade, not an
	// error — nobody installs a receiver for its captions.
	var r, w *os.File
	if media.HasElement("subparse") {
		var err error
		if r, w, err = os.Pipe(); err != nil {
			log.Printf("idle screen without captions: %v", err)
			r, w = nil, nil
		}
	} else {
		log.Print("idle screen without captions: subparse is not installed, so the " +
			"pairing code will take the screen over instead of being written on it")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	s.mu.Lock()
	s.cancel, s.done, s.feed = cancel, done, w
	s.ctx, s.started = ctx, time.Now()
	text := s.text
	s.mu.Unlock()

	desc := s.desc(w != nil)
	go func() {
		defer close(done)
		var extra []*os.File
		if r != nil {
			// Held for as long as the pipeline runs: it is the child's fd 3.
			defer r.Close()
			extra = append(extra, r)
		}
		err := media.RunPipeline(runCtx, desc, extra...)
		if err == nil || runCtx.Err() != nil {
			return
		}
		log.Printf("idle screen not shown: %v", err)
		if len(extra) == 0 {
			return
		}

		// The captions are the part that varies between GStreamer installations,
		// so they are the part to suspect — and the picture matters more than the
		// words on it. Up it goes again without them, rather than leaving the
		// television showing the console because a caption could not be arranged.
		s.mu.Lock()
		hush := s.hush
		s.feed, s.hush = nil, nil
		s.mu.Unlock()
		if hush != nil {
			hush()
		}
		w.Close()

		log.Print("putting the idle screen back up without captions: " +
			"the pairing code will take the screen over instead of being written on it")
		if err := media.RunPipeline(runCtx, s.desc(false)); err != nil && runCtx.Err() == nil {
			log.Printf("idle screen not shown: %v", err)
		}
	}()

	// A caption outliving the screen it was written on — a pairing code still
	// counting down its window while a stream started and ended — goes back up
	// with it. Written into the pipe, it waits there until the pipeline reads it.
	if text != "" {
		s.caption(text)
	}
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
	cancel, done, feed, hush := s.cancel, s.done, s.feed, s.hush
	s.cancel, s.done, s.feed, s.hush = nil, nil, nil, nil
	s.mu.Unlock()

	if hush != nil {
		hush() // nothing more to say to a screen that is going away
	}
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Print("the idle screen is taking its time to close")
	}
	if feed != nil {
		feed.Close()
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
