package sender

// The `gocast pair` command: it asks the receiver to show a code on its own
// screen and then verifies it. It runs on the transmitting machine — the one
// that shows the code is the receiver, and that part lives in
// internal/receiver.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"gocast/internal/control"
	"gocast/internal/discovery"
)

// Pair asks the receiver to show a code and then verifies it.
func Pair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	host := fs.String("host", "", "receiver IP (default: mDNS search)")
	port := fs.Int("port", control.DefaultPort, "receiver TCP port")
	name := fs.String("name", "", "name of the receiver to look for")
	wait := fs.Duration("wait", 3*time.Second, "how long the mDNS search runs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var pick discovery.Receiver
	if *host == "" {
		var err error
		if pick, err = discovery.FindReceiver(ctx, *name, *wait); err != nil {
			return err
		}
	} else {
		pick = discovery.Lookup(ctx, *host, *port, *wait)
	}
	target, tport := pick.Host, pick.Port
	log.Printf("receiver: %s (%s:%d)", pick.Name, target, tport)

	// Pairing has a token of its own too: the control port belongs to the
	// machine, and without one we would pick up the replies meant for a
	// transmission running from another terminal.
	token := control.SessionToken()

	msgs := make(chan control.Msg, 8)
	go control.ListenControl(ctx, "sender", control.SendControlPort(tport), msgs)
	time.Sleep(200 * time.Millisecond) // the socket must exist before we ask

	// We introduce ourselves before asking for the code: that is how the receiver
	// learns which token to address its replies to.
	control.SendHello(net.ParseIP(target), tport, "", token)
	control.SendPairRequest(net.ParseIP(target), tport)

	// First reply: the receiver shows the code, or refuses because it is
	// transmitting — and while transmitting it cannot show the code without
	// feeding it to whoever is watching.
	select {
	case <-ctx.Done():
		return nil
	case m, ok := <-msgs:
		if !ok {
			return errors.New("control channel unavailable")
		}
		text, got := control.SplitToken(m.Text)
		if got != token {
			return fmt.Errorf("unrecognised reply from the receiver")
		}
		switch text {
		case control.MsgNoPair:
			return errors.New("the receiver is transmitting: pairing is only possible with a " +
				"free screen, try again once the sharing has ended")
		case control.MsgShown:
			// avanti
		default:
			return fmt.Errorf("unexpected reply from the receiver: %q", text)
		}
	case <-time.After(5 * time.Second):
		return errors.New("the receiver does not answer: is it on and reachable?")
	}

	fmt.Print("Code shown on the receiver's screen. Type it here: ")
	// The error is judged on what came in, not on its own: standard input closing
	// without a newline still hands over the code typed before it, and from the
	// extension — where the dialog closes the pipe as it goes — that is the normal
	// case rather than a failure.
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	code = strings.TrimSpace(code)
	if code == "" {
		if err != nil {
			return errors.New("pairing cancelled: no code was typed")
		}
		return errors.New("no code typed")
	}

	control.SendHello(net.ParseIP(target), tport, code, token)

	select {
	case <-ctx.Done():
		return nil
	case m, ok := <-msgs:
		if !ok {
			return errors.New("control channel unavailable")
		}
		text, got := control.SplitToken(m.Text)
		if got != token {
			return fmt.Errorf("unrecognised confirmation from the receiver")
		}
		switch text {
		case control.MsgPaired:
			// Filed under the receiver's identity rather than its address, so that
			// pairing survives a change of IP.
			if err := control.RememberPin(pick.Key(), code); err != nil {
				return fmt.Errorf("pairing succeeded but was not stored: %w", err)
			}
			fmt.Printf("Paired with %s. From now on you can transmit without retyping the code.\n",
				pick.Name)
			return nil
		case control.MsgDeny:
			return errors.New("invalid or expired code")
		default:
			return fmt.Errorf("unexpected reply from the receiver: %q", text)
		}
	case <-time.After(5 * time.Second):
		return errors.New("no confirmation from the receiver")
	}
}
