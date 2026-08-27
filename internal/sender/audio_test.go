package sender

import "testing"

// gst-device-monitor prints the readable name first and the properties after,
// and everything up to the next name belongs to the device just opened. This
// is the shape the parser has to survive, preamble included.
const deviceMonitorOutput = `Probing devices...


Device found:

	name  : ThinkPad USB-C Dock Audio Stereo digitale (IEC958)
	class : Audio/Sink
	properties:
		is-default = true (gboolean)
		node.name = alsa_output.usb-Lenovo_Dock.iec958-stereo
		node.description = ThinkPad USB-C Dock Audio

Device found:

	name  : cAVS Speaker
	class : Audio/Sink
	properties:
		is-default = false (gboolean)
		node.name = alsa_output.pci-0000_00_1f.3.HiFi__Speaker__sink
`

func TestParseAudioOutputs(t *testing.T) {
	outs := parseAudioOutputs([]byte(deviceMonitorOutput))
	if len(outs) != 2 {
		t.Fatalf("outputs parsed: %d, want 2: %+v", len(outs), outs)
	}

	if outs[0].Desc != "ThinkPad USB-C Dock Audio Stereo digitale (IEC958)" {
		t.Errorf("first description: %q", outs[0].Desc)
	}
	// The properties must land on the device whose name opened the record, not
	// on the one before or after it.
	if !outs[0].Default || outs[1].Default {
		t.Errorf("the default landed on the wrong device: %+v", outs)
	}
	if outs[1].Node != "alsa_output.pci-0000_00_1f.3.HiFi__Speaker__sink" {
		t.Errorf("second node name: %q", outs[1].Node)
	}
}

// The monitor name is the audio server's convention, not ours: both PulseAudio
// and pipewire-pulse expose a sink's monitor as the sink name plus .monitor.
func TestMonitorName(t *testing.T) {
	o := audioOutput{Node: "alsa_output.usb-Lenovo_Dock.iec958-stereo"}
	if got := o.Monitor(); got != "alsa_output.usb-Lenovo_Dock.iec958-stereo.monitor" {
		t.Errorf("Monitor() = %q", got)
	}
}

// A device with no node.name cannot be captured, so printing it would be a
// line nobody can act on.
func TestParseAudioOutputsSkipsUnusable(t *testing.T) {
	outs := parseAudioOutputs([]byte("\tname  : Something\n\tclass : Audio/Sink\n"))
	if len(outs) != 0 {
		t.Errorf("kept a device with no node name: %+v", outs)
	}
}

// The sample command must show an output other than the default, or it is no
// example of choosing something else.
func TestExampleAvoidsTheDefault(t *testing.T) {
	outs := []audioOutput{
		{Desc: "dock", Node: "a", Default: true},
		{Desc: "speaker", Node: "b"},
	}
	if got := example(outs); got.Node != "b" {
		t.Errorf("example is %q, want the non-default one", got.Node)
	}
	// With only the default there, it is the only thing that can be shown.
	if got := example(outs[:1]); got.Node != "a" {
		t.Errorf("example with a single output is %q", got.Node)
	}
}
