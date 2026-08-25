package receiver

// Picking and probing the audio output device.

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
)

// alsaCardLine matches the lines of `aplay -l`: "card 1: vc4hdmi [...], device 0: ...".
var alsaCardLine = regexp.MustCompile(`^card (\d+): (\S+).*device (\d+):`)

// hdmiALSADevice returns the first device attached to HDMI that can actually
// play what the pipeline will feed it.
//
// The name matters more than it looks. On a Raspberry the HDMI card often
// accepts **only** IEC958_SUBFRAME_LE — S/PDIF subframes — and refuses plain
// S16_LE, which is what the pipeline produces. `plughw` does not bridge that
// gap: converting to IEC958 is the job of the `hdmi:` device, which wraps the
// samples through the iec958 plugin. Measured on a Pi 4:
//
//	plughw:0,0                 Sample format non available
//	hdmi:CARD=vc4hdmi,DEV=0    plays
//
// So the candidates are built from the card names and each is tried in turn.
// Guessing one shape of name and declaring defeat when it fails is how this
// receiver spent weeks mute in front of a television that worked.
func hdmiALSADevice() string {
	for _, dev := range hdmiCandidates() {
		if probeALSA(dev) {
			return dev
		}
		log.Printf("audio: %s refuses S16LE/48000/2ch, trying the next one", dev)
	}
	return ""
}

// hdmiCandidates lists the device names worth trying, best first.
func hdmiCandidates() []string {
	out, err := exec.Command("aplay", "-l").Output()
	if err != nil {
		return nil
	}

	var devs []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		low := strings.ToLower(line)
		if !strings.Contains(low, "hdmi") && !strings.Contains(low, "vc4") {
			continue
		}
		m := alsaCardLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num, name, dev := m[1], m[2], m[3]

		// hdmi: first — it is the one that goes through the iec958 plugin, and
		// on the Raspberry it is often the only one that plays at all.
		devs = append(devs,
			fmt.Sprintf("hdmi:CARD=%s,DEV=%s", name, dev),
			fmt.Sprintf("sysdefault:CARD=%s", name),
			fmt.Sprintf("plughw:%s,%s", num, dev),
		)
	}
	return devs
}

// probeALSA opens the device and feeds it a second of silence in the very
// format the pipeline will use.
//
// This is not a theoretical precaution: on the Raspberry Pi's vc4hdmi the open
// fails when the display does not advertise audio support in its EDID, and in
// GStreamer an alsasink that cannot open takes the whole pipeline down with it,
// video included. Better to find out here and give up on audio alone.
func probeALSA(dev string) bool {
	cmd := exec.Command("aplay",
		"-D", dev, "-f", "S16_LE", "-r", "48000", "-c", "2",
		"-t", "raw", "-d", "1", "/dev/zero")
	return cmd.Run() == nil
}
