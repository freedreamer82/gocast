package receiver

import "testing"

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
