package sender

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"gocast/internal/control"
	"gocast/internal/discovery"
	"gocast/internal/media"
	"gocast/internal/portal"
	"gocast/internal/version"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// capture describes the screen source: the first stretch of pipeline, any
// descriptors to hand to the child process, and the size the source declares
// (0 when unknown).
type capture struct {
	Src    string
	Extra  []*os.File
	Width  int
	Height int
	Kind   string // "screen" or "window", as the portal declares it
	Close  func()
}

// captureSource prepares the capture. Under Wayland ximagesrc grabs nothing:
// everything must go through the portal, which returns an fd to PipeWire.
func captureSource(ctx context.Context, keepalive int, sourceTypes uint32) (*capture, error) {
	if portal.IsWayland() {
		sc, err := portal.StartScreenCast(ctx, sourceTypes)
		if err != nil {
			return nil, fmt.Errorf("ScreenCast portal: %w", err)
		}
		ka := ""
		if keepalive > 0 {
			ka = fmt.Sprintf(" keepalive-time=%d", keepalive)
		}
		return &capture{
			// ExtraFiles[0] reaches the child as descriptor 3.
			//
			// keepalive-time on, and that is not a small reversal.
			//
			// pipewiresrc delivers a frame only when the screen changes. On a
			// still source — a window with a document open, a screen nobody is
			// working on — nothing comes out at all: measured, 5 kbit/s, which is
			// nothing. Whoever is watching never even sees the first frame, and
			// there is no error anywhere.
			//
			// Resending the last frame ten times a second keeps the stream alive,
			// the picture appears, and a keyframe reaches anyone who joins
			// mid-transmission within a couple of seconds. It costs bandwidth on a
			// motionless screen, but a receiver showing nothing costs far more.
			//
			// always-copy hands buffers straight back to PipeWire instead of
			// passing them downstream by reference, so the source does not run out
			// of free buffers when the encoder holds on to them.
			Src: fmt.Sprintf(
				"pipewiresrc fd=3 path=%d do-timestamp=true always-copy=true%s",
				sc.NodeID, ka),
			Extra:  []*os.File{sc.FD},
			Width:  sc.Width,
			Height: sc.Height,
			Kind:   sc.Kind,
			Close:  sc.Close,
		}, nil
	}

	if os.Getenv("DISPLAY") == "" {
		return nil, errors.New("no graphical session detected")
	}
	return &capture{Src: "ximagesrc use-damage=0", Kind: "screen", Close: func() {}}, nil
}

// scaleChain builds the stretch of pipeline that constrains the capture's
// output: a videoscale, if needed, plus a capsfilter.
//
// With an empty rectangle videoscale is not inserted at all: that is the case
// where the source is already within the limit and there is nothing to shrink.
//
// The dimensions are always **fixed**, never ranges. On a PipeWire source caps
// are fixated before the real ones are known, and leaving anything to be
// inferred sends videoscale into a division by zero:
//
//	_gst_util_uint64_scale_int: assertion 'denom > 0' failed
//	Error calculating the output scaled size - integer overflow
//
// Hence the long way round: the source is measured by grabbing a frame — the
// same mechanism used for cropping — and videoscale is handed two ready-made
// numbers.
func scaleChain(out media.Rect, pad bool) string {
	if out.Empty() {
		return "! video/x-raw,format=I420"
	}

	// add-borders fills what is left over with black instead of distorting the
	// picture: it is needed when the receiver asks for an exact frame — one of its
	// display's modes — and the source has different proportions. An ultrawide
	// inside 1920x1080 becomes 1920x804 with two bars above and below, and it is
	// those bars that make the picture acceptable to kmssink.
	borders := ""
	if pad {
		borders = " add-borders=true"
	}
	// Fixed dimensions, never ranges: on a PipeWire source fixation happens
	// before the caps are known, and with a range left to infer videoscale runs
	// into a division by zero. Given both numbers there is nothing left to work
	// out.
	return fmt.Sprintf(
		"! videoscale%s ! video/x-raw,format=I420,width=%d,height=%d,pixel-aspect-ratio=1/1",
		borders, out.W, out.H)
}

// effectiveMaxWidth combines the ceiling the receiver declares with the one the
// sender asks for.
//
// The receiver sets a limit, not a measurement: the sender may go lower to save
// bandwidth, but it cannot go above — past that value the receiver simply does
// not decode.
func effectiveMaxWidth(declared, requested int) int {
	switch {
	case declared <= 0:
		return requested // the receiver sets no limit
	case requested <= 0:
		return declared
	case requested < declared:
		return requested
	default:
		return declared
	}
}

// fitWithin shrinks a source to fit the maximum width, keeping its proportions.
// It returns an empty rectangle when there is nothing to shrink.
func fitWithin(src media.Rect, maxWidth int) media.Rect {
	if maxWidth <= 0 || src.W <= 0 || src.H <= 0 || src.W <= maxWidth {
		return media.Rect{}
	}
	return media.Rect{
		W: maxWidth,
		H: (src.H*maxWidth + src.W/2) / src.W,
	}.Even()
}

// vbvOptions caps how heavy a single frame may be.
//
// Over TCP this is a smoothing measure; over UDP it was what made the picture
// arrive at all. Without it a keyframe at 1920x1080 weighs half a megabyte and
// goes out in one burst: it leaves the PC's gigabit card faster than a receiver
// on a 100 Mbit link — a Raspberry, whose network also goes through USB — can
// absorb. One lost packet leaves the access unit incomplete and tsdemux drops
// it; and since every keyframe is large, all of them are lost. The result is a
// stream without a single keyframe: audio plays and video never appears, with
// no error anywhere.
//
// Measured on the real path: at 12000 kbit/s zero frames delivered, with this
// cap 186, at the same average bitrate.
//
// The buffer is a twentieth of the bitrate, i.e. 50 milliseconds of stream:
// large enough not to strangle ordinary frames and small enough to spread a
// keyframe over several of them.
func vbvOptions(bitrate int) string {
	buf := bitrate / 20
	if buf < 200 {
		buf = 200
	}
	return fmt.Sprintf("option-string=\"vbv-maxrate=%d:vbv-bufsize=%d\" ", bitrate, buf)
}

// effectiveKeyint turns zero into "one keyframe per second".
func effectiveKeyint(keyint, fps int) int {
	if keyint > 0 {
		return keyint
	}
	if fps > 0 {
		return fps
	}
	return 25
}

// framing describes how to fit the picture inside the receiver's frame: how
// large to draw it and how much black to put around it.
type framing struct {
	src   media.Rect // the source, as it really is
	frame media.Rect // the final size, the one the receiver can show
	inner media.Rect // the picture itself, in proportion
	l, r  int        // bars left and right
	t, b  int        // bars top and bottom
}

// letterbox computes that geometry.
//
// We work it out ourselves rather than leaving it to videoscale's add-borders,
// which on paper does exactly this. The reason is measured: with the portal's
// source videoscale gives up — "Can't calculate borders" — because the pixel
// aspect ratio it receives is unusable, and it then distorts the picture to fill
// the frame. With videotestsrc it computed the bars perfectly, which is why the
// bench test never showed the fault.
//
// Here we know every number and depend on nothing the portal declares.
func letterbox(src, frame media.Rect) framing {
	f := framing{src: src, frame: frame, inner: frame}
	if src.Empty() || frame.Empty() {
		return f
	}

	// Pick the side that reaches the edge first: the other one has room to spare,
	// and what is spare becomes a black bar.
	//
	// Rounding down loses up to two rows and makes the bars asymmetric:
	// 1440*1920/3440 is 803.7, which truncated gives 802 instead of 804.
	if src.W*frame.H > frame.W*src.H {
		f.inner = media.Rect{W: frame.W, H: media.EvenDown((src.H*frame.W + src.W/2) / src.W)}
	} else {
		f.inner = media.Rect{W: media.EvenDown((src.W*frame.H + src.H/2) / src.H), H: frame.H}
	}

	f.l = media.EvenDown((frame.W - f.inner.W) / 2)
	f.r = frame.W - f.inner.W - f.l
	f.t = media.EvenDown((frame.H - f.inner.H) / 2)
	f.b = frame.H - f.inner.H - f.t
	return f
}

// borderChain frames the picture inside the receiver's frame.
//
// The work is done by videoscale's add-borders, which on paper is the natural
// way — but on its own it gives up ("Can't calculate borders") because the caps
// arriving from the portal carry an unusable pixel aspect ratio, and the same
// log shows "invalid matrix 0 for RGB format". Having given up, it distorts.
//
// So the caps are rewritten first with capssetter — real dimensions and a 1/1
// pixel aspect ratio — and videoscale then has everything it needs to work the
// bars out by itself.
//
// videobox, which did the same thing with explicit numbers, was removed: with
// the real source it plugged the chain and not one packet went out, while with
// videotestsrc it worked — one more thing verified under the wrong conditions.
func (f framing) borderChain() string {
	if f.src.Empty() {
		return ""
	}
	// The format is forced before the label is rewritten: capssetter replaces
	// caps without looking at the data, and if videoconvert is still passing BGRA,
	// declaring it I420 corrupts the picture — GStreamer itself reports it with an
	// assertion in gst_video_frame_map_id.
	//
	// No framerate, neither here nor anywhere else in the chain. pipewiresrc
	// delivers caps without a rate. Declaring one somewhere creates an input and
	// an output that disagree, and videoscale refuses to build the converter:
	// "assertion in_info->fps_n == out_info->fps_n failed", pipeline dead in
	// negotiation, zero bytes transmitted. Measured: with the rate declared, 5
	// assertions and no data; without it, 1.7 MB over twenty frames.
	//
	// The declared rate used to keep the H264 level down, which with the portal's
	// irregular timestamps shot up to 5.2 while the Raspberry's V4L2 decoder stops
	// at 5.1. But the bitrate is held by the VBV, and the receiver copes with 5.2
	// either way: that constraint no longer exists.
	return fmt.Sprintf(
		"! video/x-raw,format=I420 "+
			"! capssetter caps=\"video/x-raw,format=I420,width=%d,height=%d,"+
			"pixel-aspect-ratio=1/1\" join=false replace=true ",
		f.src.W, f.src.H)
}

// chooseFrame decides which of the receiver's display modes to fill.
//
// Without --width it is the preferred one, which is what the receiver announces
// as its screen size. With --width it is the largest announced mode no wider
// than that — because asking a receiver for 1200 pixels of width when its
// screen has no such mode gets the stream refused in negotiation, while asking
// for 1280x720 works on any screen that lists it.
//
// A receiver that announces no mode list keeps the old behaviour: its preferred
// size, and --width ignored.
func chooseFrame(r discovery.Receiver, width int) media.Rect {
	preferred := media.Rect{W: r.MaxWidth, H: r.MaxHeight}
	if width <= 0 || preferred.Empty() || width >= preferred.W {
		return preferred
	}

	best := media.Rect{}
	for _, m := range r.Modes {
		if m.W > width || m.Empty() {
			continue
		}
		if m.W > best.W {
			best = m
		}
	}
	if best.Empty() {
		log.Printf("the receiver announces no mode at or below %d pixels wide: "+
			"staying at %dx%d", width, preferred.W, preferred.H)
		return preferred
	}
	log.Printf("--width %d: using the %dx%d mode the receiver announced", width, best.W, best.H)
	return best
}

// Probe counts the frames the source really delivers, with no network and no
// encoder in the way. It separates two faults that look identical from outside:
// a portal that produces no new frames, and a downstream chain that fails to
// pass them on.
func Probe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	keepalive := fs.Bool("keepalive", false,
		"include keepalive-time (off by default, so every frame counted is a real one)")
	stage := fs.String("stage", "capture", "how far to go: capture | scale | encode")
	width := fs.Int("width", 1920, "maximum width, as in send")
	bitrate := fs.Int("bitrate", 12000, "bitrate, as in send")
	fps := fs.Int("keyint", 10, "frames between keyframes, as in send")
	source := fs.String("source", "auto", "what to offer in the picker: auto | screen | window")
	if err := fs.Parse(args); err != nil {
		return err
	}

	types, err := portal.ParseSourceTypes(*source)
	if err != nil {
		return err
	}

	var src string
	var extra []*os.File
	closeFn := func() {}
	var srcSize media.Rect

	if portal.IsWayland() {
		sc, err := portal.StartScreenCast(ctx, types)
		if err != nil {
			return fmt.Errorf("ScreenCast portal: %w", err)
		}
		srcSize = media.Rect{W: sc.Width, H: sc.Height}
		closeFn = sc.Close
		extra = []*os.File{sc.FD}
		src = fmt.Sprintf("pipewiresrc fd=3 path=%d do-timestamp=true always-copy=true", sc.NodeID)
		if *keepalive {
			src += " keepalive-time=500"
		}
		// To be taken with a pinch of salt: when a single window is shared the
		// portal still reports the monitor's size.
		log.Printf("size declared by the portal: %dx%d", sc.Width, sc.Height)
	} else {
		src = "ximagesrc use-damage=0"
	}
	defer closeFn()

	// Each stage adds a piece of the send chain and counts there. The first stage
	// where the rate collapses is the element holding the frames back.
	chain := src + " ! videoconvert"
	switch *stage {
	case "capture":
	case "scale", "encode":
		chain += " " + scaleChain(fitWithin(srcSize, *width), false)
		if *stage == "encode" {
			chain += fmt.Sprintf(" ! queue ! x264enc tune=zerolatency "+
				"speed-preset=ultrafast bitrate=%d key-int-max=%d ! h264parse config-interval=-1",
				*bitrate, *fps)
		}
	default:
		return fmt.Errorf("unknown stage %q: use capture, scale or encode", *stage)
	}

	log.Printf("stage %q — move something on the shared screen, Ctrl-C to stop", *stage)

	desc := "-v " + chain +
		" ! fpsdisplaysink video-sink=fakesink text-overlay=false " +
		"signal-fps-measurements=true sync=false"
	return media.RunPipelineWatched(ctx, desc, false, extra...)
}

// areaPrefix introduces the line with which the sender declares, on standard
// output, which portion of the screen it is transmitting.
//
// It is for whoever transmits, not whoever watches: with the receiver in
// another room — a Raspberry attached to the TV — there is no visual feedback
// about what is ending up on somebody else's screen. The extension reads this
// line and marks exactly the shared area.
//
// On standard output rather than in the log: it is data to be consumed, not a
// message to be read, and it has to stand apart from the rest even when
// diagnostics are off.
const areaPrefix = "GOCAST-AREA "

func announceArea(spec string, bufW, bufH int, kind string) {
	area := spec
	if spec == "" || spec == "no" {
		area = fmt.Sprintf("%dx%d+0+0", bufW, bufH)
	}
	if kind == "" {
		kind = "unknown"
	}
	fmt.Printf("%s%s %s\n", areaPrefix, area, kind)
}

func Send(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	host := fs.String("host", "", "receiver IP (default: mDNS search)")
	port := fs.Int("port", control.DefaultPort, "receiver TCP port")
	bitrate := fs.Int("bitrate", 12000, "video bitrate in kbit/s")
	name := fs.String("name", "", "name of the receiver to look for")
	wait := fs.Duration("wait", 3*time.Second, "how long the mDNS search runs")
	// Three different things that had ended up sharing one number: the rate at
	// which frames are sent, the distance between keyframes, and the encoder
	// preset. Whoever joins a transmission already under way sees nothing until a
	// keyframe arrives, and the source only delivers on changes — so keyframes
	// stay frequent. Over a cable the bandwidth cost is irrelevant.
	fps := fs.Int("fps", 25, "maximum frames per second transmitted")
	keyint := fs.Int("keyint", 0,
		"frames between keyframes, 0 = one per second")
	stretch := fs.Bool("stretch", false,
		"distort the picture to fill the frame instead of letterboxing it")
	preset := fs.String("preset", "veryfast",
		"x264 preset: ultrafast is the lightest and the worst looking, "+
			"veryfast/faster cost more CPU and look far better at the same bitrate")
	// Default 0, meaning no resizing. Over a cable the native resolution is not a
	// problem, while inserting a videoscale on a PipeWire source is: it fixates
	// the dimensions before the real caps are known and fails negotiation with a
	// division by zero. It is there for a receiver that cannot take 1080p.
	width := fs.Int("width", 0, "maximum width transmitted, 0 to leave it alone")
	audio := fs.Bool("audio", true, "transmit the PC audio as well")
	audioSrc := fs.String("audio-source", "",
		"which output to transmit the sound of, as a monitor source name; "+
			"run `gocast audio` to list them (default: the active output)")
	verbose := fs.Bool("verbose", false, "diagnostic: print the caps the pipeline negotiates")
	keepalive := fs.Int("keepalive", 100,
		"milliseconds between resends of the last frame on a still source (0 = off)")
	source := fs.String("source", "auto", "what to offer in the picker: auto | screen | window")
	force := fs.Bool("force", false,
		"take over the receiver even if somebody else is already transmitting")
	pin := fs.String("pin", "", "the receiver's access code, if it requires one")
	crop := fs.String("crop", "auto",
		"crop to the useful content: auto | WIDTHxHEIGHT | no")
	// Default 0: every re-measurement costs an interruption, because the PipeWire
	// node does not allow a second consumer while the pipeline runs — tried, and
	// besides failing it brings gst-launch down.
	recrop := fs.Duration("recrop", 0,
		"how often to re-measure the crop, to follow a resized window "+
			"(0 = never; each measurement briefly interrupts the transmission)")
	alphaMin := fs.Int("alpha", 255,
		"minimum opacity for a pixel to count as window rather than shadow, 0-255")
	muxLat := fs.Int("mux-latency", 200,
		"milliseconds the muxer waits for the audio branch (only with audio on)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// One keyframe per second.
	//
	// To x264, key-int-max=0 means "no keyframes beyond the first": whoever joins
	// after the start never receives one and cannot rebuild a frame, so video
	// never appears — while audio, which needs no keyframes, plays perfectly.
	// That is exactly the symptom this fault presents with, and it produces no
	// error anywhere.
	*keyint = effectiveKeyint(*keyint, *fps)

	types, err := portal.ParseSourceTypes(*source)
	if err != nil {
		return err
	}

	var pick discovery.Receiver
	if *host == "" {
		var err error
		if pick, err = discovery.FindReceiver(ctx, *name, *wait); err != nil {
			return err
		}
	} else {
		// Even when reaching it by address we consult the announcement: we need its
		// identity, which is what the stored code is filed under.
		pick = discovery.Lookup(ctx, *host, *port, *wait)
	}
	target, tport := pick.Host, pick.Port
	log.Printf("receiver: %s (%s:%d)", pick.Name, target, tport)

	// A version gap is worth saying out loud, once, before anything else goes
	// wrong: the two halves are installed separately and drift apart without
	// warning, and the symptoms — a flag ignored, a resolution that will not
	// change — point anywhere but here.
	switch {
	case pick.Version == "":
		log.Printf("the receiver does not announce a version: it predates this "+
			"sender (v%s), so some options may be ignored", version.String())
	case pick.Version != version.String():
		log.Printf("version mismatch: receiver v%s, sender v%s — update the older side "+
			"if something behaves oddly", pick.Version, version.String())
	}

	// The receiver declares in its announcement whether it requires pairing, so
	// we can say so right away instead of transmitting for a few seconds and then
	// being turned away for no obvious reason.
	if pick.Pairing && !pick.Paired && *pin == "" {
		return fmt.Errorf("%s requires pairing: run `gocast pair --host %s` first",
			pick.Name, target)
	}

	// The code is typed once and stays: without memory it would have to be
	// supplied on every transmission, and from the GNOME extension it could not be
	// supplied at all. The key is the receiver's identity, not its address, which
	// under DHCP changes.
	key := pick.Key()
	code := *pin
	if code == "" {
		if code = control.RecallPin(key); code != "" {
			log.Printf("using the stored code for %s", pick.Name)
		}
	} else if err := control.RememberPin(key, code); err != nil {
		log.Printf("code not stored: %v", err)
	}

	// The receiver warns us when it stops watching, or when somebody else is
	// already transmitting: without that we would keep transmitting into the void,
	// or worse, two streams would mix.
	//
	// This session's token tells our notices apart from those meant for an earlier
	// run on the same machine.
	token := control.SessionToken()

	sendCtx, stopAll := context.WithCancel(ctx)
	defer stopAll()
	go control.WaitForStop(sendCtx, tport, token, func(reason string) {
		// A refused code must not be offered again: otherwise, once the code on the
		// receiver changes, the sender would retry the old one forever with nothing
		// to suggest typing a new one.
		if reason == control.StopDenied {
			if err := control.ForgetPin(key); err != nil {
				log.Printf("code not forgotten: %v", err)
			}
		}
		stopAll()
	})

	// Always introduce ourselves, even without a code: an open receiver ignores
	// the introduction, a protected one demands it before accepting data.
	control.SendHello(net.ParseIP(target), tport, code, token)

	if *force {
		log.Print("taking over the receiver")
		control.SendForce(net.ParseIP(target), tport)
		// A moment for the receiver to close the running session: without it our
		// connection would arrive while it is still busy and be refused.
		time.Sleep(500 * time.Millisecond)
	}

	cap, err := captureSource(sendCtx, *keepalive, types)
	if err != nil {
		return err
	}
	defer cap.Close()

	// The ceiling is declared by the receiver: it is the one that knows what it
	// can decode. The sender may go lower, never higher.
	limit := effectiveMaxWidth(pick.MaxWidth, *width)
	switch {
	case pick.MaxWidth > 0 && *width > pick.MaxWidth:
		log.Printf("the receiver goes no wider than %d: ignoring --width %d",
			pick.MaxWidth, *width)
	case limit > 0:
		log.Printf("maximum width %d", limit)
	default:
		log.Print("no width limit")
	}

	// The effective source is the crop when there is one, the buffer otherwise:
	// that is what the scaling is derived from, and it has to be known exactly
	// because videoscale on a PipeWire source wants fixed dimensions.
	auto := *crop == "auto"
	measure := func() (string, media.Rect) {
		full := media.Rect{W: cap.Width, H: cap.Height}
		r, err := media.DetectContent(sendCtx, cap.Src, cap.Width, cap.Height, *alphaMin, cap.Extra...)
		if err != nil {
			log.Printf("automatic cropping failed (%v): transmitting the whole buffer", err)
			return "no", full
		}
		if r.X == 0 && r.Y == 0 && r.W == cap.Width && r.H == cap.Height {
			return "no", full
		}
		return r.String(), media.Rect{W: r.W, H: r.H}
	}

	spec := *crop
	srcSize := media.Rect{W: cap.Width, H: cap.Height}
	if auto {
		spec, srcSize = measure()
		log.Printf("crop: %s (buffer %dx%d)", spec, cap.Width, cap.Height)
	}
	announceArea(spec, cap.Width, cap.Height, cap.Kind)
	audioIn := resolveAudioIn(sendCtx, *audio, *audioSrc)

	// The window can be resized mid-transmission, and the crop has to be redone.
	// videocrop cannot be reconfigured while running, so we re-measure at
	// intervals and rebuild the pipeline only when the measurement really changes:
	// stopping it every round would produce a stutter every time, even with a
	// motionless window.
	for sendCtx.Err() == nil {
		cropEl, err := cropElement(spec, cap.Width, cap.Height)
		if err != nil {
			return err
		}

		// Two different regimes. With a declared frame the output is exactly that
		// size, black bars included, because the receiver draws by setting a display
		// mode. Without one, the older width ceiling applies and the source's
		// proportions are kept.
		out := fitWithin(srcSize, limit)
		var box *framing
		if out.Empty() {
			// Nothing to shrink, but the dimensions are needed anyway: capssetter
			// only replaces caps when given all of them.
			out = srcSize
		}
		// A lower resolution is a request for a *different display mode*, not for
		// an arbitrary smaller size: the receiver draws by setting a mode and
		// refuses anything else. So --width picks the largest mode the receiver
		// announced that fits, and the picture is framed into that instead.
		if frame := chooseFrame(pick, *width); !frame.Empty() && *stretch {
			// Distorted on purpose: proportions are lost, the picture fills the
			// frame. An escape hatch for a receiver whose frame does not suit
			// letterboxing.
			out = frame
			log.Printf("source %dx%d, transmitting %dx%d distorted (--stretch)",
				srcSize.W, srcSize.H, frame.W, frame.H)
		} else if frame := chooseFrame(pick, *width); !frame.Empty() {
			f := letterbox(srcSize, frame)
			box, out = &f, frame
			log.Printf("source %dx%d, transmitting %dx%d inside the %dx%d frame (bars %d/%d)",
				srcSize.W, srcSize.H, f.inner.W, f.inner.H, frame.W, frame.H, f.t, f.l)
		} else {
			// Without a frame we transmit the source's own resolution, and that has
			// to be said loudly: a receiver drawing on HDMI accepts only its
			// display's modes, so 3440x1440 will never be shown. It happens when the
			// mDNS announcement was not read — receiver started after the sender, or
			// unreachable — and the symptom is a black screen with not one error
			// anywhere.
			log.Printf("WARNING: the receiver declared no resolution, "+
				"transmitting %dx%d as it is", out.W, out.H)
			log.Print("if the screen stays black: start the receiver first, " +
				"or force the size with --width")
		}

		desc := senderPipeline(cap.Src, cropEl, target, tport, *bitrate, *fps, *keyint,
			*preset, out, box, audioIn, *muxLat, *verbose)
		// Always, not only with --verbose: knowing which pipeline is running is the
		// first question in any diagnosis, and inferring it from the flags passed is
		// a source of misunderstandings.
		log.Printf("pipeline: %s", desc)
		log.Printf("transmitting to %s:%d — %d kbit/s — Ctrl-C to stop",
			target, tport, *bitrate)

		runCtx, cancel := context.WithCancel(sendCtx)
		var remeasure atomic.Bool
		if auto && *recrop > 0 {
			go func() {
				select {
				case <-runCtx.Done():
				case <-time.After(*recrop):
					remeasure.Store(true)
					cancel()
				}
			}()
		}

		err = media.RunPipeline(runCtx, desc, cap.Extra...)
		cancel()

		if sendCtx.Err() != nil {
			return nil
		}
		if !remeasure.Load() {
			return err
		}

		// We only re-measure now, with the pipeline stopped: the PipeWire node does
		// not allow a second consumer while transmitting, and the attempt does not
		// merely fail — it takes gst-launch down with a SIGSEGV.
		if next, nextSource := measure(); next != spec {
			log.Printf("crop changed: %s → %s", spec, next)
			spec, srcSize = next, nextSource
		}
	}
	return nil
}

// cropElement turns --crop into a videocrop, measuring the margins from the
// buffer size the portal declares.
//
// It is needed because GNOME, when a single window is shared, declares
// source_type=2 but hands over a buffer as large as the monitor, with the
// window painted at the top left and the rest black. Where the useful content
// ends up is written nowhere: the portal exposes no position, and the caps
// carry only the buffer size. Since it cannot be inferred, it is stated by
// hand.
func cropElement(spec string, bufW, bufH int) (string, error) {
	if spec == "" || spec == "no" {
		return "", nil
	}

	// The origin is optional: --crop 1280x800 means 1280x800+0+0.
	r := media.Rect{}
	if _, err := fmt.Sscanf(spec, "%dx%d+%d+%d", &r.W, &r.H, &r.X, &r.Y); err != nil {
		r.X, r.Y = 0, 0
		if _, err := fmt.Sscanf(spec, "%dx%d", &r.W, &r.H); err != nil {
			return "", fmt.Errorf(
				"--crop %q: expected WIDTHxHEIGHT or WxH+X+Y, for example 1280x800+40+20", spec)
		}
	}

	if r.W <= 0 || r.H <= 0 {
		return "", fmt.Errorf("--crop %q: the dimensions must be positive", spec)
	}
	if r.X < 0 || r.Y < 0 {
		return "", fmt.Errorf("--crop %q: the origin cannot be negative", spec)
	}
	if bufW <= 0 || bufH <= 0 {
		return "", errors.New("--crop needs the buffer size, which the portal did not declare")
	}
	if r.X+r.W > bufW || r.Y+r.H > bufH {
		return "", fmt.Errorf("--crop %s exceeds the %dx%d buffer", r, bufW, bufH)
	}
	if r.X == 0 && r.Y == 0 && r.W == bufW && r.H == bufH {
		return "", nil // same as the buffer: no element to insert
	}

	// All four sides: cropping only right and bottom would keep the top and left
	// margins inside, and shift the window in the transmitted image.
	return fmt.Sprintf("! videocrop left=%d top=%d right=%d bottom=%d ",
		r.X, r.Y, bufW-r.X-r.W, bufH-r.Y-r.H), nil
}

// muxLatency gives the aggregator time to wait for the slower branch.
//
// mpegtsmux is an aggregator: with two pads it only produces when both have
// data for the current position. The audio branch systematically arrives after
// the video one, and without this tolerance the muxer waits forever — the first
// frame goes through and then everything stops, video included, while upstream
// every element looks healthy. Measured: 1 frame without it, 235 with 50 ms;
// the default is 200 ms for margin on a loaded machine.
//
// It is only needed when audio is really there: with video alone the muxer has
// a single pad and the tolerance would be latency added for nothing.
func muxLatency(monitor string, ms int) string {
	if monitor == "" || ms <= 0 {
		return ""
	}
	return fmt.Sprintf(" latency=%d", ms*1_000_000)
}

// liveQueueMS is how much a queue in the send chain may hold, in milliseconds.
//
// A bare `queue` holds a full second, and on a live chain that second is a trap
// rather than a safety margin. Nothing here is ever timed against a clock: the
// encoder takes frames as fast as they come and the muxer emits as fast as the
// socket accepts, so a queue that fills has no mechanism to empty again. One
// transient — the encoder stumbling on a busy machine, the receiver pausing
// long enough for TCP to push back — and the queue stays full for the rest of
// the transmission. The delay it holds becomes latency you cannot get rid of
// without restarting.
//
// 200 ms absorbs the jitter a queue is actually there for and caps what a
// transient can cost.
const liveQueueMS = 200

// liveQueue builds such a queue. With leak on it drops from the head, i.e. the
// oldest frame, which on a shared screen is the frame nobody wants any more:
// what matters is what is on the screen now, not the whole history of it.
//
// Leaking is safe in front of x264enc because these are raw frames — dropping
// one costs a stutter and nothing else. It would not be safe after the encoder,
// where every frame is a reference for the ones that follow.
func liveQueue(leak bool) string {
	q := fmt.Sprintf("queue max-size-buffers=0 max-size-bytes=0 max-size-time=%d",
		liveQueueMS*1_000_000)
	if leak {
		q += " leaky=downstream"
	}
	return q
}

func senderPipeline(src, crop, host string, port, bitrate, fps, keyint int, preset string, out media.Rect, box *framing, monitor string, muxLatencyMs int, verbose bool) string {
	verb := "-q"
	if verbose {
		verb = "-v"
	}

	// format=I420 is not fussiness. The screen arrives as BGRA and videoconvert,
	// left to itself, picks Y444: x264enc then encodes High 4:4:4 predictive, a
	// profile almost no decoder handles and the Raspberry's hardware decoder does
	// not touch at all. Constraining to I420 yields a 4:2:0 profile, decodable
	// everywhere.
	//
	// No framerate constraint anywhere, and that is a deliberate choice.
	// pipewiresrc announces framerate=0/1 and delivers a frame only when the
	// screen changes. Both ways of imposing a constant rate were tried and both
	// make things worse:
	//
	//   - asking for it in the caps (framerate=30/1) makes the portal refuse the
	//     negotiation, "stream error: no more input formats", and nothing starts;
	//   - a videorate absorbs the conversion leaving the source at 0/1, and to
	//     duplicate a frame it must first receive the following one: on a
	//     motionless screen that never arrives, so the video branch emits nothing
	//     and the muxer declares audio only.
	//
	// Staying at a variable rate, the encoder emits when the source delivers: on
	// a still screen transmission drops to nothing and whoever is watching keeps
	// the last frame, picking up again as soon as something moves. For a shared
	// screen that is the right trade-off, and it uses no bandwidth when there is
	// nothing to show.
	//
	// With a frame we scale to the inner picture and let videoscale add the
	// black; without one we simply scale.
	scale, boxing := scaleChain(out, false), ""
	if box != nil {
		// Normalise the caps first, then scale with bars: the order matters, because
		// it is precisely the wrong pixel aspect ratio that makes videoscale give
		// up.
		boxing, scale = box.borderChain(), scaleChain(box.frame, true)
	}

	// TCP transport, not UDP, and it is the difference between a picture and a
	// mosaic.
	//
	// The path towards a Raspberry loses packets structurally: above 2-3 Mbit/s,
	// measured, zero errors at 2, eleven at 5, nineteen at 11 — on a 100 Mbit
	// link. Every lost packet destroys a slice of a frame, and since it stays
	// broken until the next keyframe the screen is permanently banded with
	// garbage. TCP retransmits what is lost: same content, same bitrate, a
	// flawless picture.
	//
	// The price is that if the receiver cannot keep up the sender slows down
	// instead of dropping frames, so latency can grow. On a local network, with a
	// hardware decoder at the other end, it is a price you do not notice.
	//
	// alignment=7 packs seven 188-byte TS packets per buffer, i.e. 1316 bytes. It
	// is no longer needed to fit a buffer into a datagram, but it does no harm and
	// keeps the stream compatible with a UDP receiver.
	desc := fmt.Sprintf(
		"%s %s %s! videoconvert %s%s ! %s "+
			"! x264enc tune=zerolatency speed-preset=%s bitrate=%d key-int-max=%d %s"+
			"! h264parse config-interval=-1 ! mpegtsmux name=mux alignment=7%s "+
			"! tcpclientsink host=%s port=%d",
		verb, src, crop, boxing, scale, liveQueue(true), preset, bitrate, keyint,
		vbvOptions(bitrate), muxLatency(monitor, muxLatencyMs), host, port)

	if monitor == "" {
		return desc
	}
	enc := aacEncoder()
	return desc + fmt.Sprintf(
		" pulsesrc device=%s ! audioconvert ! audioresample "+
			"! %s bitrate=128000 ! aacparse ! %s ! mux.",
		monitor, enc, liveQueue(false))
}

// resolveAudioIn picks the audio source to capture, or an empty string when it
// is not viable: without an AAC encoder there is nothing to send.
//
// When the default is used it is resolved to a name a person recognises before
// being logged. Echoing "@DEFAULT_MONITOR@" back says nothing, and this PC can
// easily have five outputs — a dock, built-in speakers, three HDMI ports. If a
// player is sending its sound to one of the others, the transmission carries
// silence and the log gives no way to notice.
func resolveAudioIn(ctx context.Context, enabled bool, src string) string {
	if !enabled {
		log.Print("audio: disabled on the command line")
		return ""
	}
	if aacEncoder() == "" {
		log.Print("audio: no AAC encoder (install gstreamer1.0-libav), transmitting video only")
		return ""
	}
	if src != "" {
		log.Printf("audio: %s via %s", src, aacEncoder())
		return src
	}

	if outs, err := audioOutputs(ctx); err == nil {
		if def, ok := defaultOutput(outs); ok {
			log.Printf("audio: transmitting what %q is playing", def.Desc)
			if len(outs) > 1 {
				log.Printf("audio: %d other outputs on this PC — if the sound is "+
					"missing, it is going to one of them: run `gocast audio`", len(outs)-1)
			}
		}
	}
	log.Printf("audio: %s via %s", defaultMonitor, aacEncoder())
	return defaultMonitor
}
