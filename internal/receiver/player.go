package receiver

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"

	"gocast/internal/media"
)

// How the work is divided, which took a night to work out:
//
//   - ffmpeg demuxes and decodes, because it tolerates lost packets;
//   - kmssink draws, because it is the only output that is actually visible on
//     this Pi.
//
// On a link that drops packets the two libraries behave in opposite ways.
// GStreamer discards an incomplete access unit: since every keyframe is large
// and travels in hundreds of consecutive packets, not one intact keyframe gets
// through, and with no keyframe there is no picture to rebuild. Audio, made of
// tiny packets, keeps playing — that is the symptom this fault presents with,
// and it produces not a single error in the logs. ffmpeg decodes what it has,
// reports "corrupt input packet" and carries on. Measured on the same stream at
// the same moment: GStreamer 0 frames, ffmpeg 814 at 23 per second.
//
// Since the transport moved to TCP nothing is lost any more, so the pure
// GStreamer path (see playbackChain) is preferable — it keeps everything in one
// process and costs about a sixth of the CPU. This player stays for lossy links
// and for receivers whose GStreamer cannot drive their decoder.
//
// ffmpeg's own SDL output looked like it worked and was an illusion: what was on
// the screen was an image left in the framebuffer by an earlier test. Checked
// against a cleared screen, SDL draws nothing. Hence the raw-video pipe into a
// GStreamer sink.

// ffmpegPlayer describes how to play with ffmpeg in front of a GStreamer sink.
type ffmpegPlayer struct {
	decoder string     // empty = software
	verbose bool       // with diagnostics ffmpeg prints the frames it delivers
	frame   media.Rect // screen size: raw frames do not carry their own dimensions
	sink    string     // the GStreamer sink that draws

	// Audio does not go through ffmpeg but through a separate GStreamer
	// pipeline, fed the same bytes.
	//
	// This is not a whim: ffmpeg's ALSA muxer cannot open the Raspberry's HDMI
	// output — "Cannot allocate memory", it does not satisfy the vc4hdmi
	// driver's buffer constraints under any of the possible device names —
	// while alsasink plays through it without trouble. Keeping them apart also
	// means a failure in audio cannot take video down, which had already
	// happened when both lived in one process.
	audioDesc string
}

func newFFmpegPlayer(audioDev string, audioPort int, verbose bool,
	frame media.Rect, sink string) *ffmpegPlayer {
	p := &ffmpegPlayer{decoder: ffmpegDecoder(), verbose: verbose,
		frame: frame, sink: sink}
	if audioDev != "" && audioPort > 0 {
		// sync=true and a generous queue in front of the sink.
		//
		// With sync=false alsasink plays as soon as data arrives, and on a bursty
		// stream it runs dry: "xrun recovery: Broken pipe" over and over, which
		// to the ear is continuous stuttering. The card needs a beat, and the
		// clock gives it one, together with a few tenths of a second of slack
		// built up before it starts.
		p.audioDesc = fmt.Sprintf(
			"-q fdsrc fd=0 ! tsdemux name=d "+
				"d. ! audio/mpeg ! queue ! aacparse ! avdec_aac "+
				"! audioconvert ! audioresample "+
				"! audio/x-raw,format=S16LE,rate=48000,channels=2 "+
				"! queue max-size-time=600000000 max-size-buffers=0 max-size-bytes=0 "+
				"! alsasink device=%s sync=true "+
				"d. ! video/x-h264 ! queue ! fakesink sync=false async=false",
			audioDev)
	}
	return p
}

// ffmpegAvailable reports whether ffmpeg is installed.
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// ffmpegDecoder looks for a hardware decoder among the ones this ffmpeg lists.
func ffmpegDecoder() string {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-decoders").Output()
	if err != nil {
		return ""
	}
	for _, name := range []string{"h264_v4l2m2m", "h264_omx"} {
		if strings.Contains(string(out), name) {
			return name
		}
	}
	return ""
}

func (p *ffmpegPlayer) args() []string {
	level := "warning"
	if p.verbose {
		// At this level ffmpeg prints "frame=N": the only way to know how many
		// frames actually arrive without looking at the screen.
		level = "info"
	}
	a := []string{"-hide_banner", "-loglevel", level, "-stats",
		// No buffering: this is a live screen, not a film.
		"-fflags", "nobuffer", "-flags", "low_delay"}
	if p.decoder != "" {
		a = append(a, "-c:v", p.decoder)
	}

	// From standard input: the bytes are handed over by the Go process, which
	// took them from the sender's TCP connection.
	a = append(a, "-nostdin", "-i", "pipe:0", "-map", "0:v",
		// No scaling filter: the sender already fills the declared frame exactly,
		// and forcing it here cost a software rescale on every frame — four cores
		// at 100% on a Pi 3, to do nothing at all.
		//
		// Incoming timestamps are thrown away and regenerated. A shared screen
		// arrives at irregular intervals and several frames end up with the same
		// stamp; the output then discards them as "non monotonically increasing
		// dts", and since it discards nearly all of them the last one through
		// stays frozen on screen — it looks as if nothing is arriving while the
		// decoder is working perfectly. passthrough is not enough, because it
		// keeps the duplicate stamps; drop removes them and lets the output
		// generate valid ones.
		"-fps_mode", "drop",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
	return a
}

// displayArgs builds the gst-launch that draws the raw frames.
func (p *ffmpegPlayer) displayArgs() []string {
	return media.SplitPipeline(fmt.Sprintf(
		"-q fdsrc ! rawvideoparse width=%d height=%d format=i420 framerate=25/1 ! %s",
		p.frame.W, p.frame.H, unsynced(p.sink)))
}

func (p *ffmpegPlayer) describe() string {
	dec := p.decoder
	if dec == "" {
		dec = "software"
	}
	out := fmt.Sprintf("ffmpeg (%s) -> %s", dec, p.sink)
	if p.audioDesc != "" {
		out += ", audio via alsasink"
	}
	return out
}

// run starts ffmpeg for video and, when there is one, the audio pipeline
// alongside it.
//
// Only video is waited on: it is what decides when playback is over. Audio
// lives in its own context and dies with it, and if it fails we simply say so —
// killing video over an audio fault is the flaw this separation exists to
// avoid.
func (p *ffmpegPlayer) run(ctx context.Context, src io.Reader) error {
	dec := exec.CommandContext(ctx, "ffmpeg", p.args()...)
	dec.Stderr = media.ErrOut

	show := exec.CommandContext(ctx, "gst-launch-1.0", p.displayArgs()...)
	show.Stderr = media.ErrOut

	// Raw frames travel from the decoder to the drawer through a pipe.
	raw, err := dec.StdoutPipe()
	if err != nil {
		return err
	}
	show.Stdin = raw

	// The incoming stream goes to the decoder and, when there is one, to the
	// audio pipeline as well: two separate processes, so an audio fault does not
	// switch the video off.
	toVideo, err := dec.StdinPipe()
	if err != nil {
		return err
	}
	dsts := []io.Writer{toVideo}

	var audio *exec.Cmd
	if p.audioDesc != "" {
		audio = exec.CommandContext(ctx, "gst-launch-1.0", media.SplitPipeline(p.audioDesc)...)
		audio.Stderr = media.ErrOut
		toAudio, err := audio.StdinPipe()
		if err != nil {
			return err
		}
		dsts = append(dsts, toAudio)
		if err := audio.Start(); err != nil {
			log.Printf("audio not started: %v", err)
			dsts = dsts[:1]
		}
	}

	if err := show.Start(); err != nil {
		return fmt.Errorf("starting the sink: %w", err)
	}
	if err := dec.Start(); err != nil {
		return fmt.Errorf("starting the decoder: %w", err)
	}

	// Copy for as long as the sender transmits, then close the inputs.
	copyErr := copyTo(src, dsts...)
	for _, d := range dsts {
		if c, ok := d.(io.Closer); ok {
			c.Close()
		}
	}

	// Waiting plainly is not safe: a pipeline can take the EOS and simply sit
	// there, and while it lives the session never ends — see runWithSource.
	waitOrKill(dec, endGrace)
	waitOrKill(show, endGrace)
	if audio != nil {
		waitOrKill(audio, endGrace)
	}
	return copyErr
}
