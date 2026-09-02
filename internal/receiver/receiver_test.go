package receiver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gocast/internal/media"
)

// The fallback must step down one configuration at a time and stop at the end,
// never starting over: a decoder the stream has already disproved has no reason
// to be retried, and cycling back would keep the chain from ever reaching the
// one that works.
func TestPlaybackChainStepsDownOnce(t *testing.T) {
	c := &playbackChain{configs: []playbackConfig{
		{"v4l2h264dec", ""},
		{"v4l2h264dec", "! videoconvert "},
		{"avdec_h264", "! videoconvert "},
	}}

	var seen []playbackConfig
	seen = append(seen, c.current())
	for c.next() {
		seen = append(seen, c.current())
	}

	if len(seen) != len(c.configs) {
		t.Fatalf("configurations walked: %d, want %d", len(seen), len(c.configs))
	}
	for i, cfg := range seen {
		if cfg != c.configs[i] {
			t.Errorf("position %d: %+v, want %+v", i, cfg, c.configs[i])
		}
	}
	if c.next() {
		t.Error("the chain started over past the last configuration")
	}
}

// With a single decoder available there is nothing to fall back to, and the
// chain has to say so instead of retrying the same thing forever.
func TestPlaybackChainWithoutAlternatives(t *testing.T) {
	c := &playbackChain{configs: []playbackConfig{{"avdec_h264", "! videoconvert "}}}
	if c.next() {
		t.Error("offered an alternative that does not exist")
	}
}

// No bare queue may survive in the playback chain.
//
// The two bare ones held a second each and were most of a three-second delay
// measured on a real link: with every sink on sync=false nothing in this
// pipeline ever drops a late frame, so a queue that fills once stays full for
// the rest of the session.
func TestPlaybackChainHasNoUnboundedQueue(t *testing.T) {
	desc := receiverPipeline("kmssink", "hdmi:CARD=vc4hdmi,DEV=0", false, false,
		"v4l2h264dec", "", defaultAudioLatency)

	if strings.Contains(desc, "! queue !") {
		t.Errorf("a bare queue is back in the chain:\n%s", desc)
	}
	if !strings.Contains(desc, "video/x-h264 ! queue max-size-buffers=0 "+
		"max-size-bytes=0 max-size-time=200000000") {
		t.Errorf("the video queue is not bounded:\n%s", desc)
	}
	// The audio queue takes half the audio budget, not the generic bound.
	if !strings.Contains(desc, "max-size-time=75000000 leaky=downstream") {
		t.Errorf("the audio queue does not carry half of the 150 ms budget:\n%s", desc)
	}
}

// The video queue must not leak, and this is the opposite of the rule on the
// sender.
//
// Here the buffers are encoded frames: each one is a reference for those that
// follow, so dropping one puts garbage on screen until the next keyframe — up
// to a full second. Blocking instead pushes back through TCP until the sender's
// queue drops a raw frame, which costs nothing at all. Audio is different: AAC
// frames stand alone, so leaking there costs a click and is worth it.
func TestVideoQueueBlocksAndAudioQueueLeaks(t *testing.T) {
	desc := receiverPipeline("kmssink", "hdmi:CARD=vc4hdmi,DEV=0", false, false,
		"v4l2h264dec", "", defaultAudioLatency)

	video, _, ok := strings.Cut(desc, "d. ! audio/mpeg")
	if !ok {
		t.Fatalf("no audio branch in the description:\n%s", desc)
	}
	if strings.Contains(video, "leaky") {
		t.Errorf("the video queue leaks: it will show garbage until the next keyframe:\n%s", video)
	}
	if !strings.Contains(desc, "audio/mpeg ! queue max-size-time=200000000 leaky=downstream") &&
		!strings.Contains(desc, "leaky=downstream") {
		t.Errorf("the audio queue does not leak:\n%s", desc)
	}
}

// The ALSA ring must be stated, not left to GstAudioBaseSink's 200 ms.
//
// With the video sink drawing on arrival, that default ring is pure skew: the
// picture is ahead of the sound by the whole depth of it.
func TestALSARingIsStatedAndFloored(t *testing.T) {
	desc := receiverPipeline("kmssink", "hdmi:CARD=vc4hdmi,DEV=0", false, false,
		"v4l2h264dec", "", 200)
	for _, want := range []string{"buffer-time=100000", "latency-time=25000"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the ALSA ring does not carry %q:\n%s", want, desc)
		}
	}

	// Below the floor the card runs dry on the first late thread, and stuttering
	// is a worse fault than the lateness it would save.
	if got := alsaBufferProps(10); got != " buffer-time=50000 latency-time=12500" {
		t.Errorf("alsaBufferProps(10) = %q, want the 50 ms floor", got)
	}

	// Zero must state nothing at all. The vc4hdmi refuses buffer constraints it
	// does not like, and a silent television needs a way back to the driver's own
	// configuration without a rebuild.
	if got := alsaBufferProps(0); got != "" {
		t.Errorf("alsaBufferProps(0) = %q, want the driver's defaults left alone", got)
	}
	bare := receiverPipeline("kmssink", "hdmi:CARD=vc4hdmi,DEV=0", false, false,
		"v4l2h264dec", "", 0)
	if strings.Contains(bare, "buffer-time") || strings.Contains(bare, "latency-time") {
		t.Errorf("--audio-latency 0 still constrains the card:\n%s", bare)
	}
}

// --audio-latency must govern the whole audio budget, not just the ring.
//
// What you hear lagging behind the picture is the sum of the two places sound
// waits: the queue in front of the decoder and the ALSA ring. A knob that moves
// only one of them cannot close the gap, and the one it leaves alone is the
// bigger of the two.
func TestAudioLatencyGovernsQueueAndRingTogether(t *testing.T) {
	desc := receiverPipeline("kmssink", "hdmi:CARD=vc4hdmi,DEV=0", false, false,
		"v4l2h264dec", "", 400)

	for _, want := range []string{
		"max-size-time=200000000 leaky=downstream", // the queue's half
		"buffer-time=200000",                       // the ring's half
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("the 400 ms budget does not produce %q:\n%s", want, desc)
		}
	}
}

// The pairing code is a caption over the idle screen, not a screen of its own:
// tearing the picture down and putting it back up blinks the television and
// changes its mode, twice, to show four digits.
func TestIdleScreenCaptions(t *testing.T) {
	s := &idleScreen{sink: "kmssink", frame: media.Rect{W: 1920, H: 1080},
		image: "/tmp/idle.png"}

	desc := s.desc(true)
	for _, want := range []string{
		"textoverlay name=cap",  // the overlay the caption lands on
		"wait-text=false",       // or the picture stops until something is said
		"fdsrc fd=3 ! subparse", // the pipe the cues travel down
		"cap.text_sink",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("a captioned idle screen is missing %q:\n%s", want, desc)
		}
	}
	if got := s.desc(false); strings.Contains(got, "fdsrc") {
		t.Errorf("without a pipe there must be nothing to read from:\n%s", got)
	}

	// Nothing on screen: the caller has to be told, so that it can light a screen
	// of its own rather than believe the code is up.
	if s.caption("Pairing code: 1234") {
		t.Error("a screen that is down must not claim to have shown the code")
	}
	// Remembered even so, and cleared on the way out: a code that outlives its
	// screen has to come back with it, and a spent one must not.
	if s.text != "Pairing code: 1234" {
		t.Errorf("the caption was not remembered: %q", s.text)
	}
	s.caption("")
	if s.text != "" {
		t.Errorf("the caption was not cleared: %q", s.text)
	}
}

// The caption is written as subtitle cues, and a malformed one is not rejected
// by anything: subparse simply drops it, and the code never appears.
func TestCueFormat(t *testing.T) {
	got := cue(7, 3661*time.Second+250*time.Millisecond, "Pairing code: 2040")
	want := "7\n01:01:01,250 --> 01:01:02,250\nPairing code: 2040\n\n"
	if got != want {
		t.Errorf("cue:\n%q\nwant:\n%q", got, want)
	}
	// The blank line at the end is what closes a cue: without it subparse waits
	// for the rest of a subtitle that never comes.
	if !strings.HasSuffix(got, "\n\n") {
		t.Error("a cue must end with a blank line")
	}
}

// A GStreamer that will not arrange the captions must still paint the picture.
// A television left showing the console because a caption could not be had is a
// far worse failure than a pairing code that takes the screen over, and it is
// the kind of difference that only appears on somebody else's installation.
func TestIdleScreenFallsBackWithoutCaptions(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	// A gst-launch that refuses anything to do with subtitles, and records what
	// it was asked to run.
	stub := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in *subparse*) exit 1;; esac; done\n" +
		"echo \"$@\" >> " + ran + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "gst-launch-1.0"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gst-inspect-1.0"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Ahead of the real one, not instead of it: the stub still needs a shell and
	// a sleep to stay alive the way a pipeline does.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	s := &idleScreen{sink: "kmssink", frame: media.Rect{W: 1920, H: 1080},
		image: "/tmp/idle.png"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.hide()

	s.show(ctx)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out, err := os.ReadFile(ran)
		if err != nil || !strings.Contains(string(out), "kmssink") {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if strings.Contains(string(out), "subparse") {
			t.Fatalf("the pipeline that ran still carries the captions:\n%s", out)
		}
		// And the caller has to be told, or it would believe a code written into
		// a pipe nobody reads is up on the screen.
		if s.caption("Pairing code: 1234") {
			t.Error("a screen with no captions must not claim to have shown the code")
		}
		return
	}
	t.Fatal("the idle screen never went back up without captions")
}
