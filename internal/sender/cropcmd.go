package sender

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"

	"gocast/internal/media"
	"gocast/internal/portal"
)

// The `gocast crop` diagnostic: it measures automatic cropping on a real source
// and prints the result, transmitting nothing. It lives with the sender because
// the sender is the only side that knows how to open the portal.

// Crop shows how the buffer handed over by the portal is made: how much of it
// is opaque, how much is penumbra, and which rectangle each rule produces.
//
// It is there to choose a crop by looking at the pixels instead of assuming how
// the compositor paints the window.
func Crop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crop", flag.ExitOnError)
	source := fs.String("source", "window", "what to offer in the picker: auto | screen | window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	types, err := portal.ParseSourceTypes(*source)
	if err != nil {
		return err
	}
	if !portal.IsWayland() {
		return errors.New("this diagnostic only applies to portal capture under Wayland")
	}

	sc, err := portal.StartScreenCast(ctx, types)
	if err != nil {
		return fmt.Errorf("ScreenCast portal: %w", err)
	}
	defer sc.Close()

	src := fmt.Sprintf("pipewiresrc fd=3 path=%d do-timestamp=true always-copy=true", sc.NodeID)
	frame, err := media.CaptureFrame(ctx, src, sc.Width, sc.Height, sc.FD)
	if err != nil {
		return err
	}

	fmt.Printf("buffer declared by the portal: %dx%d\n\n", sc.Width, sc.Height)
	dumpNodeProps(sc.NodeID)

	// How many pixels in each opacity band: it says whether the window is painted
	// solid, translucent, or with soft edges.
	var transparent, penumbra, opaque int
	for i := 3; i < len(frame); i += 4 {
		switch a := frame[i]; {
		case a == 0:
			transparent++
		case a == 255:
			opaque++
		default:
			penumbra++
		}
	}
	tot := len(frame) / 4
	fmt.Println("opacity distribution:")
	fmt.Printf("  alpha 0     (transparent): %8d  %5.1f%%\n", transparent, pct(transparent, tot))
	fmt.Printf("  alpha 1-254 (penumbra)   : %8d  %5.1f%%\n", penumbra, pct(penumbra, tot))
	fmt.Printf("  alpha 255   (opaque)     : %8d  %5.1f%%\n\n", opaque, pct(opaque, tot))

	fmt.Println("rectangle measured by each rule (WIDTHxHEIGHT+X+Y):")
	for _, min := range []int{1, 64, 128, 200, 250, 255} {
		fmt.Printf("  alpha >= %3d : %s\n", min,
			describeRect(media.BoundingBox(frame, sc.Width, sc.Height, media.OpaqueAbove(min))))
	}
	fmt.Printf("  non-black pixels: %s\n",
		describeRect(media.BoundingBox(frame, sc.Width, sc.Height, media.NotBlack)))

	fmt.Println("\nThe origin matters as much as the size: content not anchored at the top")
	fmt.Println("left has to be cropped from there too. Pass it to send with --crop WxH+X+Y,")
	fmt.Println("or pick the matching threshold with --alpha.")
	return nil
}

func describeRect(r media.Rect) string {
	if r.Empty() {
		return "none (the rule selects nothing)"
	}
	return r.String()
}

// dumpNodeProps shows the properties of the PipeWire node carrying the stream.
//
// The portal declares very little — id, size, source type — but the node is
// created by Mutter, which does know the window: if a title or an identifier
// exists anywhere, it is here. This is the last place to look for an identity
// of the shared window, after the portal and Mutter's API both turned out to be
// silent.
func dumpNodeProps(nodeID uint32) {
	out, err := exec.Command("pw-cli", "info", fmt.Sprint(nodeID)).CombinedOutput()
	if err != nil {
		fmt.Printf("PipeWire node properties unreadable (%v)\n\n", err)
		return
	}

	fmt.Printf("properties of PipeWire node %d:\n", nodeID)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// The interesting properties are the ones that might name the window; the
		// rest of the dump is formats and buffers.
		if strings.Contains(line, "=") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()
}

func pct(n, tot int) float64 {
	if tot == 0 {
		return 0
	}
	return float64(n) * 100 / float64(tot)
}
