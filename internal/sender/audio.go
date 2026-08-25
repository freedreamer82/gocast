package sender

import (
	"gocast/internal/media"
)

// defaultMonitor is the magic name the audio server resolves on its own into
// the monitor of the active sink — that is, whatever the PC is playing now.
//
// We use it instead of querying pactl because the pactl binary is not installed
// by default on PipeWire-only systems, while the magic name is handled
// server-side by pipewire-pulse.
const defaultMonitor = "@DEFAULT_MONITOR@"

// aacEncoder picks the first available AAC encoder.
func aacEncoder() string {
	for _, e := range []string{"avenc_aac", "voaacenc", "faac", "fdkaacenc"} {
		if media.HasElement(e) {
			return e
		}
	}
	return ""
}
