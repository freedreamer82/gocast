package receiver

import (
	"context"
	"flag"
	"fmt"
	"gocast/internal/control"
	"gocast/internal/discovery"
	"gocast/internal/media"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"
)

// receiverPipeline builds the playback pipeline.
//
// The capsfilters right after tsdemux are not decoration: the demuxer's pads
// appear at runtime, and without explicit constraints gst-launch can wire the
// video into the audio branch (a queue accepts any caps). With video/x-h264 and
// audio/mpeg the routing is deterministic.
func receiverPipeline(sink, audioDev string, stats, verbose bool,
	dec, conv string) string {

	// The audio branch always exists, even when nothing is played: if nobody
	// consumes the audio pad, the demuxer's queue fills up and stalls video.
	//
	// async=false is not a detail: when the sender transmits video only, this
	// branch never links and its sink receives no data, so it never prerolls.
	// With async=true (the default) the pipeline waits for that preroll and video
	// stops after the first frame, while everything else — network, demuxer,
	// decoder — looks perfectly healthy. Measured: 0 frames with async=true, 237
	// with async=false.
	audio := "d. ! audio/mpeg ! queue ! aacparse ! fakesink sync=false async=false"
	if audioDev != "" {
		audio = fmt.Sprintf(
			"d. ! audio/mpeg ! queue ! aacparse ! avdec_aac "+
				"! audioconvert ! audioresample "+
				"! audio/x-raw,format=S16LE,rate=48000,channels=2 "+
				"! alsasink device=%s sync=false async=false", audioDev)
	}

	// With --stats the sink is replaced by fpsdisplaysink, which writes the frame
	// count over the picture: it tells you at a glance whether the stream is
	// still arriving or the last frame received is simply frozen.
	if stats {
		// signal-fps-measurements sends the counts to the terminal as well: without
		// it the number is only superimposed on the picture and never reaches the
		// logs.
		sink = "fpsdisplaysink text-overlay=true signal-fps-measurements=true"
	}

	// -v only on request. It makes gst-launch install a notification on every
	// property change of every element, and on a live pipeline the cost is not
	// negligible: through a pipe it is enough to throttle the stream, to the point
	// where video stops after the first frame. We saw it by comparing the same
	// pipeline launched by hand with -q, which runs.
	//
	// The price is that without --verbose the detection of an arriving stream
	// does not work: those are the lines that report it. Better to lose a
	// diagnostic convenience than the video.
	verb := "-q"
	if verbose || stats {
		verb = "-v"
	}

	// fdsrc on standard input: the Go process owns the TCP connection and hands
	// the bytes over, which is also how it knows when the transmission ends.
	//
	// No tsparse: tsdemux takes the transport stream as it comes. With
	// set-timestamps=true, tsparse recomputes the timestamps from the PCR, and on
	// a sparse stream — which a shared screen necessarily is, since frames only
	// arrive on changes — that recomputation can stall the demuxer after the
	// first packets. Measured: without it the chain decodes 236 frames where
	// before a single one got through.
	return fmt.Sprintf(
		"%s fdsrc fd=0 ! tsdemux name=d "+
			"d. ! video/x-h264 ! queue ! h264parse ! %s %s! %s "+
			"%s",
		verb, dec, conv, unsynced(sink), audio)
}

// unsynced turns clock synchronisation off, unless whoever passed --sink has
// already decided for themselves.
//
// It is not there to unblock playback: measured, a sink with sync=true yields
// the same number of frames. It is there to remove latency — for a live shared
// screen it is better to draw frames as they arrive than to hold them back to
// respect a timeline that comes from another computer, on a clock we do not
// share.
func unsynced(sink string) string {
	if strings.Contains(sink, "sync=") {
		return sink
	}
	return sink + " sync=false"
}

// playbackChain is the list of playback configurations to try, in order of
// preference, plus the one in use.
//
// It exists because the choice of decoder cannot be settled in advance: knowing
// that an element is installed says nothing about whether it will cope with the
// stream, and testing it locally would mean manufacturing a fake one. So we
// start from the best candidate and step down only when the real stream proves
// it wrong.
type playbackChain struct {
	sink, audioDev string
	stats, verbose bool

	configs []playbackConfig
	i       int

	// When set, playback goes through ffmpeg and the GStreamer configurations are
	// not even tried: see player.go for why.
	ff *ffmpegPlayer
}

// A configuration is a decoder plus the conversion towards the sink, if any:
// hardware decoders often hand over a format the sink takes as is — and on a
// Pi 3 a videoconvert at 1080p costs a whole core — but it is not guaranteed,
// so both variants stay among the attempts.
type playbackConfig struct{ dec, conv string }

func newPlaybackChain(sink, audioDev string, stats, verbose bool,
	pinned string) *playbackChain {
	c := &playbackChain{sink: sink, audioDev: audioDev,
		stats: stats, verbose: verbose}

	// A decoder pinned by hand excludes the others. It is needed where detection
	// cannot get there on its own: on some Raspberry setups v4l2h264dec is
	// installed, declares itself capable and decodes not a single frame — the
	// driver is marked staging by the kernel itself — so the chain tries it on
	// every start and drops it on every start, losing a few seconds of
	// transmission each time.
	candidates := media.DecoderCandidates()
	if pinned != "" {
		candidates = []string{pinned}
	}

	for _, dec := range candidates {
		if media.HardwareDecoder(dec) {
			// First without conversion: if it links, that is the cheapest path.
			c.configs = append(c.configs,
				playbackConfig{dec, ""}, playbackConfig{dec, "! videoconvert "})
			continue
		}
		c.configs = append(c.configs, playbackConfig{dec, "! videoconvert "})
	}
	return c
}

func (c *playbackChain) empty() bool { return c.ff == nil && len(c.configs) == 0 }

// describe says what playback is going through, for the log.
func (c *playbackChain) describe() string {
	if c.ff != nil {
		return c.ff.describe()
	}
	return c.current().dec
}

// run plays the stream, through ffmpeg when one was set up and through
// GStreamer otherwise.
func (c *playbackChain) run(ctx context.Context, src io.Reader) error {
	if c.ff != nil {
		return c.ff.run(ctx, src)
	}

	// The GStreamer path: a single process that demuxes, decodes and draws
	// without raw frames having to travel through a pipe. TCP transport makes it
	// viable again — ffmpeg was there to tolerate lost packets, and over TCP none
	// are lost. Measured on the same stream: 14% of one core here against 80%
	// with ffmpeg and the raw pipe in between.
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", media.SplitPipeline(c.desc())...)
	cmd.Stdin = src
	cmd.Stdout = media.ErrOut
	cmd.Stderr = media.ErrOut
	return cmd.Run()
}

func (c *playbackChain) current() playbackConfig { return c.configs[c.i] }

func (c *playbackChain) desc() string {
	cfg := c.current()
	return receiverPipeline(c.sink, c.audioDev, c.stats, c.verbose, cfg.dec, cfg.conv)
}

// next steps to the following configuration, if there is one.
func (c *playbackChain) next() bool {
	// With ffmpeg there is nothing to fall back to: it is the fallback.
	if c.ff != nil || c.i+1 >= len(c.configs) {
		return false
	}
	c.i++
	cfg := c.current()
	if cfg.conv == "" {
		log.Printf("switching to decoder %s", cfg.dec)
	} else {
		log.Printf("switching to decoder %s with conversion", cfg.dec)
	}
	return true
}

func Serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", control.DefaultPort, "TCP port to listen on")
	sink := fs.String("sink", "", "GStreamer video sink (default: detected)")
	name := fs.String("name", "", "announced name (default: hostname)")
	audio := fs.Bool("audio", true, "play the audio that arrives")
	audioDev := fs.String("audio-device", "", "ALSA device (default: detected HDMI)")
	player := fs.String("player", "auto",
		"what plays the stream: auto | ffmpeg | gstreamer")
	decoder := fs.String("decoder", "",
		"decoder to use (default: detected, falling back automatically if it cannot cope)")
	stats := fs.Bool("stats", false, "diagnostic: show frames per second over the picture")
	verbose := fs.Bool("verbose", false, "diagnostic: print the caps the pipeline negotiates")
	// The receiver is the only one that knows what its decoder can take, and it
	// says so in its announcement: a Raspberry Pi 3 stops at 1080p, and without
	// that figure somebody transmitting from an ultrawide would send it 3440
	// pixels of width it cannot decode.
	maxWidth := fs.Int("max-width", 0,
		"widest picture this receiver can decode, 0 = taken from the screen")
	maxHeight := fs.Int("max-height", 0,
		"screen height, 0 = detected")
	pairing := fs.Bool("pairing", false,
		"require pairing: the code appears on this screen, so only somebody in front of it can read it")
	pairWindow := fs.Duration("pair-window", time.Minute,
		"how long the pairing code stays valid and on screen")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Both playback paths draw through a GStreamer sink, so it is detected here
	// once. The probe costs about ten seconds and a mode change on the screen,
	// which is why it is not repeated later.
	if *sink == "" {
		*sink = media.AutoSink()
	}
	*audioDev = resolveAudioOut(*audio, *audioDev)

	id, err := control.ReceiverID()
	if err != nil {
		// Without an identity we carry on: pairing will fall back to the address,
		// which is the earlier behaviour.
		log.Printf("receiver identity unavailable: %v", err)
	}

	// The screen resolution is not a convenience limit, it is a constraint:
	// kmssink sets a display mode and refuses everything else. Given only a
	// maximum width, the sender keeps its source's aspect ratio and produces an
	// arbitrary height — from an ultrawide that is 1920x804, which no monitor has
	// among its modes.
	if mode := media.DisplayMode(); !mode.Empty() {
		if *maxWidth == 0 {
			*maxWidth = mode.W
		}
		if *maxHeight == 0 && *maxWidth == mode.W {
			*maxHeight = mode.H
		}
		log.Printf("screen detected: %dx%d", mode.W, mode.H)
	}

	switch {
	case *maxWidth > 0 && *maxHeight > 0:
		log.Printf("declaring a %dx%d frame: the sender fills it with black bars",
			*maxWidth, *maxHeight)
	case *maxWidth > 0:
		log.Printf("declaring a maximum width of %d", *maxWidth)
	}

	srv, err := discovery.Announce(*name, *port, *pairing, id, *maxWidth, *maxHeight)
	if err != nil {
		return fmt.Errorf("mDNS announcement: %w", err)
	}
	defer srv.Shutdown()

	if *pairing {
		log.Print("pairing required: new senders must run `gocast pair`")
	} else {
		log.Print("no pairing required: anybody on the network may transmit")
	}

	chain := newPlaybackChain(*sink, *audioDev, *stats, *verbose, *decoder)

	// ffmpeg only when asked for, or when GStreamer has no decoder at all.
	//
	// It used to be the default, back when the transport was UDP and GStreamer
	// showed nothing where ffmpeg showed everything. Over TCP nothing is lost, so
	// the single-process GStreamer path wins: same picture, about a sixth of the
	// CPU, because raw frames never leave the process.
	if *player == "ffmpeg" || (*player == "auto" && len(chain.configs) == 0 && ffmpegAvailable()) {
		if !ffmpegAvailable() {
			return fmt.Errorf("ffmpeg is not installed: try\n" +
				"  sudo apt install ffmpeg")
		}
		// No local port: the stream arrives on the TCP connection and is handed to
		// both processes on standard input.
		audioPort := 0
		if *audioDev != "" {
			audioPort = 1 // a placeholder: it only says audio was asked for
		}
		frame := media.Rect{W: *maxWidth, H: *maxHeight}
		if frame.Empty() {
			frame = media.DisplayMode()
		}
		if frame.Empty() {
			frame = media.Rect{W: 1920, H: 1080}
		}
		chain.ff = newFFmpegPlayer(*audioDev, audioPort, *verbose, frame, *sink)
	}
	if chain.empty() {
		return fmt.Errorf("no H264 decoder installed: try\n" +
			"  sudo apt install gstreamer1.0-libav")
	}
	if *decoder != "" && !media.HasElement(*decoder) {
		return fmt.Errorf("decoder %q is not installed on this machine", *decoder)
	}

	if chain.ff != nil {
		log.Printf("listening on TCP %d — %s", *port, chain.describe())
		screen := &pairScreen{sink: *sink, ctx: ctx}
		arb := control.NewArbiter(ctx, *port, *pairing, *pairWindow, screen.show, screen.hide)
		return serveStreamTCP(ctx, *port, chain, *verbose || *stats, arb)
	}

	desc := chain.desc()
	if missing := media.MissingElements(desc); len(missing) > 0 {
		return fmt.Errorf("missing GStreamer elements: %s\n"+
			"This installation cannot play anything. The elements live in different\n"+
			"packages depending on the distribution — alsasink, for instance, is in\n"+
			"gstreamer1.0-alsa on Debian 13 and inside plugins-base on Debian 11. Find\n"+
			"out who provides it with:\n"+
			"  apt-file search /gstreamer-1.0/libgst   # first: sudo apt install apt-file && sudo apt-file update\n"+
			"If instead the element is there but not seen, the plugin registry is\n"+
			"corrupt: rm -rf ~/.cache/gstreamer-1.0",
			strings.Join(missing, ", "))
	}
	log.Printf("listening on TCP %d — video %q via %q", *port, *sink, chain.current().dec)
	if len(chain.configs) > 1 {
		log.Printf("%d more fallback configurations if the stream does not start",
			len(chain.configs)-1)
	}
	log.Printf("pipeline: %s", desc)

	// The pairing code is shown on the very screen that will host the content,
	// and that is the point: whoever cannot see it cannot pair.
	screen := &pairScreen{sink: *sink, ctx: ctx}
	arb := control.NewArbiter(ctx, *port, *pairing, *pairWindow, screen.show, screen.hide)

	return serveStreamTCP(ctx, *port, chain, *verbose || *stats, arb)
}

// resolveAudioOut decides which ALSA device to play on, or returns an empty
// string when audio is not viable. It always explains why: a television that is
// silent for no apparent reason is the kind of thing people lose hours on.
func resolveAudioOut(enabled bool, dev string) string {
	if !enabled {
		log.Print("audio: disabled on the command line")
		return ""
	}
	if dev == "" {
		dev = hdmiALSADevice()
	}
	if dev == "" {
		log.Print("audio: no HDMI card in `aplay -l`, carrying on without audio")
		return ""
	}
	if !probeALSA(dev) {
		log.Printf("audio: %s does not accept S16LE/48000/2ch — the display probably "+
			"does not advertise audio support in its EDID. Carrying on without audio.", dev)
		return ""
	}
	log.Printf("audio: %s", dev)
	return dev
}
