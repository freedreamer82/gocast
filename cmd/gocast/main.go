// gocast — screen sharing over a wired network.
//
// Decoding and drawing are done by GStreamer (and optionally ffmpeg), which on
// a Raspberry Pi is the only way to reach the hardware decoder. This program
// handles what neither of them does: finding the receiver on the network,
// negotiating screen capture with the Wayland portal, carrying the stream over
// TCP, and keeping the pipelines alive.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gocast/internal/discovery"
	"gocast/internal/media"
	"gocast/internal/receiver"
	"gocast/internal/sender"
	"gocast/internal/version"
)

func usage() {
	fmt.Fprint(os.Stderr, `gocast — screen sharing over a wired network

  gocast serve [--port N] [--sink DESC] [--name NAME]
        On the machine wired to the TV: announces itself over mDNS, accepts
        the stream and puts it on screen. Restarts its pipeline on its own.

  gocast send [--host IP] [--port N] [--bitrate KBIT] [--name NAME]
        On the PC: finds the receiver, captures the screen and transmits.
        Detects by itself whether the session is X11 or Wayland.

  gocast list
        Lists the receivers visible on the network.

  gocast pair [--host IP]
        Pairs with a receiver that asks for it: the code appears on its own
        screen and is typed in here. Needed once per PC, and impossible
        while somebody is transmitting.

  gocast audio
        Lists this PC's audio outputs and the monitor name of each, to hand
        to send --audio-source. The default transmits whatever the active
        output is playing: when a player is sending its sound elsewhere the
        transmission is silent, and this says which one to pick instead.

  gocast probe
        Diagnostic: counts the frames the screen source actually delivers,
        with no network and no encoder. Tells a portal that produces nothing
        apart from a downstream chain that fails to pass frames on.

  gocast version
        Prints the version of this binary. The receiver announces its own,
        and gocast list shows both when they differ.

  gocast crop
        Diagnostic: shows how the portal's buffer is made — how much of it is
        opaque, how much is shadow — and which rectangle each cropping rule
        would pick.

`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	media.SetupLogging()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = receiver.Serve(ctx, os.Args[2:])
	case "send":
		err = sender.Send(ctx, os.Args[2:])
	case "list":
		err = discovery.List(ctx, os.Args[2:])
	case "probe":
		err = sender.Probe(ctx, os.Args[2:])
	case "audio":
		err = sender.Audio(ctx, os.Args[2:])
	case "crop":
		err = sender.Crop(ctx, os.Args[2:])
	case "pair":
		err = sender.Pair(ctx, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("gocast " + version.String())
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "gocast: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	// Ctrl-C is not an error: it is the normal way to end a session.
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "gocast: %v\n", err)
		os.Exit(1)
	}
}
