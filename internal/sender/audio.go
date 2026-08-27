package sender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"strings"

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

// An output of this PC, and the source that captures what it is playing.
type audioOutput struct {
	Desc    string `json:"desc"`    // what a person reads: "ThinkPad USB-C Dock Audio (IEC958)"
	Node    string `json:"node"`    // what the audio server calls it
	Default bool   `json:"default"` // the one @DEFAULT_MONITOR@ resolves to

	// Monitor is carried in the JSON rather than left to be rebuilt on the
	// other side: the ".monitor" convention belongs to the audio server, and a
	// shell extension reassembling it by hand would be a second place to fix
	// when it ever changes.
	MonitorName string `json:"monitor"`
}

// Monitor is the source name to hand to --audio-source.
//
// The convention is the sink's own name with .monitor appended, and it is the
// audio server's, not ours: both PulseAudio and pipewire-pulse expose the
// monitor of a sink under exactly that name.
func (o audioOutput) Monitor() string { return o.Node + ".monitor" }

// audioOutputs lists the outputs of this PC.
//
// Through gst-device-monitor rather than pactl, and that is the whole reason
// this function can exist: pactl is not installed on a PipeWire-only system —
// it is why the default is the magic @DEFAULT_MONITOR@ name in the first place
// — while gst-device-monitor ships with the GStreamer tools gocast already
// needs to run at all.
//
// Audio/Sink and not Audio/Source: the sources are the microphones. What we
// want is what the PC is *playing*, which is a sink, captured through its
// monitor.
func audioOutputs(ctx context.Context) ([]audioOutput, error) {
	out, err := exec.CommandContext(ctx, "gst-device-monitor-1.0", "Audio/Sink").Output()
	if err != nil {
		return nil, fmt.Errorf("gst-device-monitor-1.0: %w", err)
	}
	return parseAudioOutputs(out), nil
}

// parseAudioOutputs reads what gst-device-monitor prints.
//
// The device's readable name arrives first, on its own "name  :" line, and the
// properties that follow belong to it until the next one. So the name opens a
// record and the properties fill it in — anything before the first name is the
// tool's own preamble and is skipped.
func parseAudioOutputs(out []byte) []audioOutput {
	var outs []audioOutput
	cur := -1

	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "name  :"):
			outs = append(outs, audioOutput{
				Desc: strings.TrimSpace(strings.TrimPrefix(line, "name  :")),
			})
			cur = len(outs) - 1
		case cur < 0:
			// preamble
		case strings.HasPrefix(line, "node.name ="):
			outs[cur].Node = strings.TrimSpace(strings.TrimPrefix(line, "node.name ="))
		case strings.HasPrefix(line, "is-default ="):
			outs[cur].Default = strings.Contains(line, "true")
		}
	}

	// A device with no node.name cannot be captured and would only be a line
	// nobody can act on.
	var keep []audioOutput
	for _, o := range outs {
		if o.Node == "" {
			continue
		}
		o.MonitorName = o.Node + ".monitor"
		keep = append(keep, o)
	}
	return keep
}

// defaultOutput returns the output @DEFAULT_MONITOR@ resolves to, so the log
// can say which one it is instead of echoing the magic name back.
func defaultOutput(outs []audioOutput) (audioOutput, bool) {
	for _, o := range outs {
		if o.Default {
			return o, true
		}
	}
	return audioOutput{}, false
}

// Audio lists this PC's audio outputs, so that --audio-source can be given
// something other than the default.
//
// It exists because the default captures the monitor of whatever output the
// system considers active, and when a player is sending its sound somewhere
// else — a dock, one of three HDMI ports, the built-in speakers — the
// transmission carries silence with nothing anywhere to say why.
func Audio(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audio", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output (used by the GNOME extension)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	outs, err := audioOutputs(ctx)

	// With --json a valid array always comes out, empty included, so the parser
	// on the other side has no special cases — the same contract as list --json.
	// An audio server that cannot be reached is not an error there: it is a
	// menu with nothing but the default in it.
	if *asJSON {
		if outs == nil {
			outs = []audioOutput{}
		}
		b, mErr := json.Marshal(outs)
		if mErr != nil {
			return mErr
		}
		fmt.Println(string(b))
		return nil
	}

	if err != nil {
		return fmt.Errorf("cannot list the audio outputs: %w\n"+
			"gst-device-monitor-1.0 comes with gstreamer1.0-tools", err)
	}
	if len(outs) == 0 {
		return fmt.Errorf("no audio output found: is the audio server running?")
	}

	fmt.Println("This PC's audio outputs. gocast transmits what one of them is")
	fmt.Println("playing, captured through its monitor:")
	fmt.Println()
	for _, o := range outs {
		mark := "  "
		if o.Default {
			mark = "*"
		}
		fmt.Printf(" %s %s\n   %s\n\n", mark, o.Desc, o.Monitor())
	}
	fmt.Println(" * is the current default, the one transmitted when")
	fmt.Println("   --audio-source is not given. To pick another:")
	fmt.Println()
	// An example built from an output that is *not* the default: suggesting the
	// default is no example at all of choosing something else.
	fmt.Printf("   gocast send --audio-source %s\n", example(outs).Monitor())
	return nil
}

// example picks the output to show in the sample command: the first that is
// not the default, or the only one there is.
func example(outs []audioOutput) audioOutput {
	for _, o := range outs {
		if !o.Default {
			return o
		}
	}
	return outs[0]
}
