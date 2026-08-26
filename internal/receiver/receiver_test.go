package receiver

import (
	"strings"
	"testing"
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
