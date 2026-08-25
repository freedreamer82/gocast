package receiver

// Picking and probing the audio output device.

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// alsaCardLine matches the lines of `aplay -l`: "card 1: vc4hdmi [...], device 0: ...".
var alsaCardLine = regexp.MustCompile(`^card (\d+): (\S+).*device (\d+):`)

// hdmiALSADevice looks through the ALSA cards for the one wired to HDMI. It
// returns a plughw device rather than hw so that ALSA performs the format
// conversions itself.
func hdmiALSADevice() string {
	out, err := exec.Command("aplay", "-l").Output()
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		low := strings.ToLower(line)
		if !strings.Contains(low, "hdmi") && !strings.Contains(low, "vc4") {
			continue
		}
		if m := alsaCardLine.FindStringSubmatch(line); m != nil {
			return fmt.Sprintf("plughw:%s,%s", m[1], m[3])
		}
	}
	return ""
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
