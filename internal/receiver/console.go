package receiver

import "os"

// blankConsole wipes the text console, so that what shows through between two
// pipelines is black instead of a login prompt.
//
// The gap itself cannot be closed from here, and it is worth being precise about
// why: the display has a single master. The idle screen has to let go of it
// before playback can set a mode — that ordering is deliberate, and without it
// playback sets no mode at all and draws nothing — and in between the kernel
// brings its framebuffer console back. What the console has on it is therefore
// what the television shows for that moment, and a console cleared once has
// nothing on it.
//
// /dev/tty0 and not /dev/tty1: it is whichever console is on screen now, which
// is the one the kernel will bring back. Needs root, which the receiver has —
// it cannot set a display mode without it — and where it has not, there is
// nothing to tidy and nothing to report.
func blankConsole() {
	f, err := os.OpenFile("/dev/tty0", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()

	// Clear, home the cursor, hide it. The cursor matters: left blinking in the
	// corner of an otherwise black screen, it is the one thing still visible.
	_, _ = f.WriteString("\033[2J\033[H\033[?25l")
}
