package media

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrOut is where messages go: standard error, plus a file when diagnostics
// have been asked for. See SetupLogging.
var ErrOut io.Writer = os.Stderr

// logEnv is the variable that asks for messages to be mirrored to a file.
//
// It exists to diagnose failures that only show up when the program is launched
// from the GNOME extension: there the output ends up in a gnome-shell pipe,
// which surfaces a single line in a notification, and the real error stays
// invisible.
//
// It is explicit rather than automatic: always writing to a shared file in /tmp
// is intrusive, and someone using the program normally has no reason to find
// one there.
const logEnv = "GOCAST_LOG"

// SetupLogging mirrors messages to the file named by GOCAST_LOG, if set.
// The value "1" is shorthand for /tmp/gocast.log.
func SetupLogging() {
	path := os.Getenv(logEnv)
	switch path {
	case "":
		return
	case "1", "true":
		path = "/tmp/gocast.log"
	}

	// Appending, not truncating: the file is shared by every run, and clearing
	// it at startup wipes precisely the log of the previous run — the one you
	// are typically trying to understand.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logPath := path
	ErrOut = io.MultiWriter(os.Stderr, f)
	log.SetOutput(ErrOut)
	log.Printf("messages mirrored to %s", logPath)
}

// SplitPipeline splits a pipeline description into arguments, honouring quotes.
//
// strings.Fields is not enough: a property containing spaces — a text to draw
// on screen, a font name — would be broken into separate arguments and the
// pipeline would not build.
func SplitPipeline(desc string) []string {
	var args []string
	var cur strings.Builder
	quoted := false

	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}

	for _, r := range desc {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// RunPipeline launches gst-launch-1.0 as a child process. Files passed in extra
// reach the child starting at descriptor 3.
func RunPipeline(ctx context.Context, desc string, extra ...*os.File) error {
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", SplitPipeline(desc)...)
	cmd.Env = gstEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = ErrOut
	cmd.ExtraFiles = extra
	return cmd.Run()
}

// RunPipelineWatched does what RunPipeline does, but reads gst-launch's output
// and turns it into readable events: which resolution came in, whether a track
// is missing, when something refused to negotiate.
func RunPipelineWatched(ctx context.Context, desc string, verbose bool, extra ...*os.File) error {
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", SplitPipeline(desc)...)
	cmd.Env = gstEnv()
	cmd.ExtraFiles = extra

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	w := &watcher{verbose: verbose}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.scan(stdout) }()
	go func() { defer wg.Done(); w.scan(stderr) }()
	wg.Wait()

	return cmd.Wait()
}

// gstEnv turns on GStreamer's minimum diagnostic level.
//
// At level 2 only errors and warnings come out: the cost is nil and the gain
// large, because negotiation failures surface as a generic "Internal data
// stream error" blamed on the source, while the real cause — the element that
// refused the data — sits in a warning that is not printed without this.
// Having it always on means the first failure is already diagnosable, without
// asking whoever hit it to reproduce the run.
func gstEnv() []string {
	if os.Getenv("GST_DEBUG") != "" {
		return nil // the caller has already decided: respect it
	}
	return append(os.Environ(), "GST_DEBUG=2")
}

var (
	// Two different shapes: gst-launch's own messages start with the word, while
	// GST_DEBUG lines start with a timestamp and a thread, with the severity in
	// the middle.
	reProblem = regexp.MustCompile(`(?i)(^(error|warning|critical)|\bWARN\b|\bERROR\b)`)
	reSize    = regexp.MustCompile(`width=\(int\)(\d+).*height=\(int\)(\d+)`)

	// tsdemux creates its pads at runtime: if the stream carries only one of the
	// two tracks, the matching branch never links and GStreamer reports it like
	// this. On its own the message does not say which branch, and it alarms for
	// no reason when audio is absent, which is perfectly normal.
	reDelayedLink = regexp.MustCompile(`(?i)failed delayed linking`)
)

type watcher struct {
	verbose    bool
	mu         sync.Mutex
	sawStream  bool
	sawVideo   bool
	warnedLink bool
}

func (w *watcher) scan(r io.Reader) {
	// Whatever happens to the scanner, the output must keep being consumed: if
	// we stop reading, the pipe fills up and gst-launch blocks on the write,
	// stalling the whole pipeline. A read error would then turn into a frozen
	// picture, with no message explaining it.
	defer io.Copy(io.Discard, r) //nolint:errcheck

	sc := bufio.NewScanner(r)
	// GStreamer caps are very long lines: the MPEG-TS streamheader one goes well
	// past the default 64 KiB buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		// The delayed-linking message is rewritten in milestone, where we can say
		// which track is missing: the raw one adds nothing.
		noisy := reDelayedLink.MatchString(line)
		if w.verbose || (!noisy && reProblem.MatchString(strings.TrimSpace(line))) ||
			strings.Contains(line, "rendered:") {
			fmt.Fprintln(ErrOut, line)
		}
		w.milestone(line)
	}
}

// milestone recognises the two moments that matter and announces each once.
func (w *watcher) milestone(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.sawStream && strings.Contains(line, "video/mpegts") {
		w.sawStream = true
		log.Print("stream arriving from the sender")
	}
	if !w.sawVideo && strings.Contains(line, "video/x-h264") {
		w.sawVideo = true
		if m := reSize.FindStringSubmatch(line); m != nil {
			log.Printf("video received: %sx%s", m[1], m[2])
		} else {
			log.Print("video received")
		}
	}

	// An unlinked branch means a missing track. Which of the two can be inferred
	// from what has already arrived — but only with --verbose, because that is
	// where the caps lines come through: without it w.sawVideo is false even
	// when video is flowing perfectly, and the inference turns into a false
	// alarm in exactly the normal case.
	if reDelayedLink.MatchString(line) && !w.warnedLink {
		w.warnedLink = true
		switch {
		case w.sawVideo:
			log.Print("the stream carries no audio")
		case w.verbose:
			log.Print("WARNING: the stream carries no video — the sender is transmitting audio only")
		default:
			log.Print("the stream carries a single track (usually: video without audio)")
		}
	}
}

// HasElement reports whether this GStreamer installation has the element.
func HasElement(name string) bool {
	return exec.Command("gst-inspect-1.0", name).Run() == nil
}

// findElement returns the first installed element whose name matches: names
// vary between GStreamer versions, the function does not.
func findElement(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	out, err := exec.Command("gst-inspect-1.0").Output()
	if err != nil {
		return ""
	}
	// Each line reads "plugin:  name: description": the name is the second
	// field, with a trailing colon to strip.
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if name := strings.TrimSuffix(f[1], ":"); re.MatchString(name) {
			return name
		}
	}
	return ""
}

// The video sinks able to draw without a display server, in order of
// preference. The order is knowledge — kmssink uses the accelerator, fbdevsink
// writes into the framebuffer by hand — but availability is not assumed: it is
// tested.
//
// A list is needed all the same, because "ends in sink" does not tell apart
// what draws from what throws away (fakesink, filesink): those would pass any
// check while showing a black screen.
var videoSinks = []string{
	"kmssink force-modesetting=true",
	"glimagesink",
	"fbdevsink",
	"autovideosink",
}

// Detection costs throwaway pipelines: run it once.
var (
	sinkOnce sync.Once
	sinkName string
)

// AutoSink picks the first sink that actually works on this machine.
//
// Testing instead of inferring: the presence of /dev/dri does not imply that
// kmssink is installed, and glimagesink can be present and fail anyway — with
// no display server EGL initialisation does not succeed ("EGL_NOT_INITIALIZED").
// A check on the name cannot tell the two apart; a trial pipeline can.
func AutoSink() string {
	sinkOnce.Do(func() { sinkName = pickSink() })
	return sinkName
}

func pickSink() string {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return "autovideosink" // a display server is running: the DRM is its own
	}

	for _, sink := range videoSinks {
		if elementWorks(sink) {
			return sink
		}
		// Say so: a rejected sink explains why we end up on a worse one, and
		// without this line the choice looks arbitrary.
		log.Printf("%s is not usable on this machine, trying the next one", sink)
	}
	log.Print("no usable video sink: the picture cannot be shown")
	return "fakesink"
}

// DecoderCandidates lists the installed H264 decoders, fastest first.
//
// None of them is tested here. Testing them would mean manufacturing an H264
// stream on the receiver, and manufacturing one would need an encoder — which
// on a receiver serves no other purpose, may be missing entirely, and on a Pi 3
// shares the silicon with the decoder, so that the test took away from the
// decoder the very resource it was measuring.
//
// The right test bench is the sender's stream: it is the real one, at the real
// resolution, and it arrives anyway. Whatever cannot cope with it is dropped
// there.
//
// The names are not guessable either: the same hardware decoder is called
// v4l2h264dec on recent GStreamer, v4l2video10dec on 1.18 — after the device
// number — and omxh264dec on the Raspberry's legacy stack.
func DecoderCandidates() []string {
	names := []string{"v4l2h264dec"}
	if dec := findElement(`^v4l2video\d+dec$`); dec != "" {
		names = append(names, dec)
	}
	names = append(names, "omxh264dec", "avdec_h264")

	var installed []string
	for _, dec := range names {
		if HasElement(dec) {
			installed = append(installed, dec)
		}
	}
	return installed
}

// elementWorks checks that a sink can take a frame and show it.
//
// The trial frame is the size of the screen, not videotestsrc's convenient
// default. kmssink draws by setting a display mode and refuses anything that is
// not a real mode: probing it at 320x240 makes it look broken when it works
// perfectly, and the receiver falls back to fbdevsink, throwing acceleration
// away. It is the same mistake that killed the 1920x804 stream for a day,
// applied to the probe instead of to the video.
func elementWorks(sink string) bool {
	// With no detected mode we still try 1920x1080: that is a mode which exists
	// everywhere, while videotestsrc's convenient 320x240 is no display's mode at
	// all, and probing kmssink with it always makes it look broken.
	m := DisplayMode()
	if m.Empty() {
		m = Rect{W: 1920, H: 1080}
	}
	size := fmt.Sprintf("! video/x-raw,format=I420,width=%d,height=%d ", m.W, m.H)
	return probe("videotestsrc num-buffers=1 ! videoconvert " + size + "! " + sink)
}

// How long a trial pipeline is given.
//
// Generous on purpose: kmssink has to set a display mode, and a television
// takes a few seconds to sync. With a tight limit the trial expires before the
// sink is ready, the sink is declared unusable, and the receiver falls back to
// glimagesink, which without a display server draws nothing: from outside you
// see a started receiver and a black screen.
const probeTimeout = 20 * time.Second

// probe runs a throwaway pipeline and reports whether it ran to completion.
func probe(desc string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gst-launch-1.0", SplitPipeline("-q "+desc)...)
	return cmd.Run() == nil
}

// HardwareDecoder reports whether the chosen decoder is accelerated: hardware
// ones hand over a format the sink already accepts, and a videoconvert in
// between would cost a whole core on a Pi 3.
func HardwareDecoder(dec string) bool {
	return strings.HasPrefix(dec, "v4l2") || strings.HasPrefix(dec, "omx")
}

// MissingElements lists the elements named in a description that this GStreamer
// installation does not have.
//
// It exists because gst-launch discovers a missing element only when it builds
// the pipeline, and we build ours on the first packet: the error then appears
// once transmission has started, from the wrong side of the room, and whoever
// is transmitting only sees that nothing happens. Checking at startup moves the
// discovery to where somebody can read it.
func MissingElements(desc string) []string {
	var missing []string
	seen := map[string]bool{}

	for _, tok := range SplitPipeline(desc) {
		// A description also contains properties (name=value), caps
		// (video/x-h264), pad references (d.) and options (-q): an element name
		// is what is none of those.
		if strings.ContainsAny(tok, "=/.,!") || strings.HasPrefix(tok, "-") {
			continue
		}
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if !HasElement(tok) {
			missing = append(missing, tok)
		}
	}
	return missing
}
