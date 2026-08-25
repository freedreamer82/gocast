package receiver

import (
	"context"
	"fmt"
	"gocast/internal/control"
	"io"
	"log"
	"net"
	"os/exec"
	"time"
)

// serveStreamTCP accepts the sender's connection and hands its bytes to the
// player.
//
// The transport is TCP rather than UDP for a measured reason: towards this kind
// of receiver the network drops packets above two or three Mbit/s, and every
// lost packet wrecks a slice of a frame which stays broken until the next
// keyframe — the screen turns into a mosaic. With TCP whatever is lost gets
// retransmitted and the picture arrives whole.
//
// It also simplifies the lifecycle: a connection closing *is* the end of the
// transmission, stated by the protocol. With UDP the end had to be guessed from
// prolonged silence, and the in-flight datagrams of a sender that had just been
// dismissed would reopen the session by themselves.
func serveStreamTCP(ctx context.Context, port int, chain *playbackChain,
	verbose bool, arb *control.Arbiter, idle *idleScreen) error {

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close() // unblocks the wait for a connection
	}()

	// Connections are accepted continuously, even while a session is running:
	// that is the only way to tell whoever arrives second "I am already
	// transmitting". Left in the accept queue, they would hang with no
	// explanation until the first one finished.
	conns := make(chan net.Conn)
	go func() {
		defer close(conns)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case conns <- c:
			case <-ctx.Done():
				c.Close()
				return
			}
		}
	}()

	// Something on the screen while waiting: without it the TV shows the console,
	// and a receiver that is ready looks exactly like one that has crashed.
	idle.show(ctx)
	defer idle.hide()

	for ctx.Err() == nil {
		conn, ok := <-conns
		if !ok {
			return nil
		}

		ip := conn.RemoteAddr().(*net.TCPAddr).IP
		if !arb.Allowed(ip) {
			log.Printf("refused %s: access code missing or wrong", ip)
			control.SendDeny(ip, port, arb.TokenFor(ip))
			conn.Close()
			continue
		}

		log.Printf("stream arriving from %s", ip)

		// The idle screen holds the sink, and therefore the display: it has to be
		// gone before playback starts, not merely asked to go.
		idle.hide()
		arb.Busy.Store(true)
		runCtx, cancel := context.WithCancel(ctx)

		// Whoever turns up mid-transmission is dismissed at once rather than left
		// waiting: they know they were refused and why. To take over they have to
		// ask over the control channel, which is the branch just below.
		go func() {
			for {
				select {
				case c, ok := <-conns:
					if !ok {
						return
					}
					other := c.RemoteAddr().(*net.TCPAddr).IP
					log.Printf("refused a second sender (%s): transmission in progress from %s",
						other, ip)
					control.SendBusy(other, port, arb.TokenFor(other))
					c.Close()
				case <-runCtx.Done():
					return
				}
			}
		}()

		// Taking over closes the running connection: it is the most direct way to
		// interrupt whoever is transmitting, and their sender notices immediately
		// because its write fails.
		go func() {
			select {
			case other := <-arb.Force:
				if !other.Equal(ip) && arb.Allowed(other) {
					log.Printf("%s is taking over: interrupting %s", other, ip)
					conn.Close()
				}
			case <-runCtx.Done():
			}
		}()

		if err := chain.run(runCtx, conn); err != nil && runCtx.Err() == nil {
			log.Printf("playback ended: %v", err)
		}

		cancel()
		conn.Close()
		arb.Busy.Store(false)

		// The sender may not have noticed the close: we say so over the control
		// channel too, so it stops instead of retrying.
		control.SendBye(ip, port, arb.TokenFor(ip))

		if ctx.Err() == nil {
			log.Print("stream ended, waiting for the next sender")
			idle.show(ctx)
		}
	}
	return nil
}

// copyTo delivers the same stream to several consumers, and survives the death
// of one of them.
//
// It exists because video and audio are two separate processes: if audio dies,
// video has to carry on. Writing to an io.MultiWriter instead, the first error
// would stop everything — which is the very fault this separation exists to
// avoid.
func copyTo(src io.Reader, dsts ...io.Writer) error {
	buf := make([]byte, 64*1024)
	alive := make([]bool, len(dsts))
	for i := range alive {
		alive[i] = true
	}

	for {
		n, err := src.Read(buf)
		if n > 0 {
			for i, d := range dsts {
				if !alive[i] {
					continue
				}
				if _, werr := d.Write(buf[:n]); werr != nil {
					alive[i] = false
					if i == 0 {
						return werr // the first is video: without it there is no session
					}
					log.Printf("a consumer of the stream stopped: %v", werr)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// runWithSource feeds src into the process's standard input and makes sure the
// process actually ends when the stream does.
//
// Closing the input is not enough. gst-launch receives the EOS and, with these
// sinks, can simply sit there: measured, a playback pipeline alive at zero CPU
// two minutes after the sender had gone. And while it lives the session never
// ends — the receiver still believes it is busy, refuses the next sender as a
// second one, and never puts its idle screen back. Nothing in the logs says so.
//
// So the end of the stream is treated as an order, not a suggestion: the input
// is closed, the process is given a moment to bow out on its own, and if it does
// not it is killed.
func runWithSource(cmd *exec.Cmd, src io.Reader, extra ...io.Writer) error {
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	dsts := append([]io.Writer{in}, extra...)
	copyErr := copyTo(src, dsts...)

	for _, d := range dsts {
		if c, ok := d.(io.Closer); ok {
			c.Close()
		}
	}
	if err := waitOrKill(cmd, endGrace); err != nil && copyErr == nil {
		return err
	}
	return copyErr
}

// How long a pipeline is given to end by itself once its input is closed.
const endGrace = 3 * time.Second

func waitOrKill(cmd *exec.Cmd, grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		log.Print("the pipeline did not end when the stream closed: stopping it")
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return nil
	}
}
