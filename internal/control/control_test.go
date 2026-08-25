package control

import (
	"context"
	"net"
	"testing"
	"time"
)

// freePort finds a data port whose two control ports — by convention the next
// two — are free as well.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		p, err := freeUDPPort()
		if err != nil {
			t.Fatalf("no free port: %v", err)
		}
		a, errA := net.ListenUDP("udp", &net.UDPAddr{Port: recvControlPort(p)})
		if errA != nil {
			continue
		}
		b, errB := net.ListenUDP("udp", &net.UDPAddr{Port: SendControlPort(p)})
		a.Close()
		if errB != nil {
			continue
		}
		b.Close()
		return p
	}
	t.Skip("no free run of three consecutive ports")
	return 0
}

// senderStoppedBy starts listening on the sender side and returns a channel
// that closes when the sender is stopped.
const testToken = "abc123"

func senderStoppedBy(t *testing.T, ctx context.Context, port int) chan struct{} {
	t.Helper()
	stopped := make(chan struct{})
	go WaitForStop(ctx, port, testToken, func(string) { close(stopped) })
	time.Sleep(100 * time.Millisecond) // the control socket has to exist first
	return stopped
}

func TestByeStopsSender(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := senderStoppedBy(t, ctx, port)
	SendBye(net.IPv4(127, 0, 0, 1), port, testToken)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the sender did not stop when the receiver closed")
	}
}

func TestBusyStopsSender(t *testing.T) {
	// A second sender must not insist: two transport streams mixed into the same
	// pipeline produce garbage, and whoever is watching would have no idea why.
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := senderStoppedBy(t, ctx, port)
	SendBusy(net.IPv4(127, 0, 0, 1), port, testToken)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the sender did not stop when the receiver refused it")
	}
}

func TestControlIgnoresOtherTraffic(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := senderStoppedBy(t, ctx, port)

	c, err := net.DialUDP("udp", nil,
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: SendControlPort(port)})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("something else")); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	select {
	case <-stopped:
		t.Fatal("stopped by a message that was none of its business")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestForceReachesReceiver(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan Msg, 4)
	go ListenControl(ctx, "receiver", recvControlPort(port), msgs)
	time.Sleep(100 * time.Millisecond)

	SendForce(net.IPv4(127, 0, 0, 1), port)

	select {
	case m := <-msgs:
		if m.Text != MsgForce {
			t.Errorf("want %q, got %q", MsgForce, m.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the takeover request never reached the receiver")
	}
}

// paired builds an arbiter with pairing on and a code already on screen,
// touching neither network nor display.
func paired(t *testing.T, code string, window time.Duration) *Arbiter {
	t.Helper()
	isolateConfig(t)
	return &Arbiter{
		pairing:  true,
		window:   window,
		accepted: map[string]bool{},
		known:    map[string]bool{},
		pending:  code,
		until:    time.Now().Add(window),
	}
}

func TestArbiterAccess(t *testing.T) {
	const port = 1 // no socket is opened: only the logic is exercised
	ip := net.IPv4(192, 168, 1, 42)

	t.Run("without pairing anybody may transmit", func(t *testing.T) {
		a := &Arbiter{known: map[string]bool{}}
		if !a.Allowed(ip) {
			t.Error("an open receiver must turn nobody away")
		}
	})

	t.Run("the code on screen pairs", func(t *testing.T) {
		a := paired(t, "1234", time.Minute)
		if a.Allowed(ip) {
			t.Error("an address that has not paired must not get through")
		}
		a.greet(ip, "1234", port)
		if !a.Allowed(ip) {
			t.Error("the code shown on screen must pair")
		}
	})

	t.Run("the wrong code does not pair", func(t *testing.T) {
		a := paired(t, "1234", time.Minute)
		a.greet(ip, "9999", port)
		if a.Allowed(ip) {
			t.Error("the wrong code must not pair")
		}
	})

	t.Run("an expired code does not pair", func(t *testing.T) {
		// The window is the time it takes to copy the code off the screen, not the
		// lifetime of a password.
		a := paired(t, "1234", time.Minute)
		a.until = time.Now().Add(-time.Second)
		a.greet(ip, "1234", port)
		if a.Allowed(ip) {
			t.Error("an expired code must not pair")
		}
	})

	t.Run("the code is good once only", func(t *testing.T) {
		// It is cleared after use: anybody intercepting it could not reuse it to
		// pair a second machine.
		a := paired(t, "1234", time.Minute)
		a.greet(ip, "1234", port)
		if a.pending != "" {
			t.Error("the code shown should have been consumed")
		}
	})

	t.Run("somebody already paired is recognised", func(t *testing.T) {
		a := paired(t, "1234", time.Minute)
		a.greet(ip, "1234", port)
		other := net.IPv4(192, 168, 1, 43)
		a.greet(other, "1234", port) // same code, already among the accepted ones
		if !a.Allowed(other) {
			t.Error("a code already paired must keep working")
		}
	})
}

func TestPairingRequiresFreeScreen(t *testing.T) {
	isolateConfig(t)
	ip := net.IPv4(127, 0, 0, 1)

	newOne := func() (*Arbiter, *bool) {
		shown := false
		a := &Arbiter{
			pairing:  true,
			window:   time.Minute,
			accepted: map[string]bool{},
			known:    map[string]bool{},
		}
		a.show = func(string) { shown = true }
		return a, &shown
	}

	t.Run("with a free screen the code appears", func(t *testing.T) {
		a, shown := newOne()
		a.startPairing(context.Background(), ip, 1)
		if !*shown || a.pending == "" {
			t.Error("the code should have been shown")
		}
	})

	t.Run("during a transmission it does not", func(t *testing.T) {
		// The screen is showing the shared content: the code would land in front of
		// whoever is watching — exactly who pairing has to keep out.
		a, shown := newOne()
		a.Busy.Store(true)
		a.startPairing(context.Background(), ip, 1)
		if *shown {
			t.Error("the code must not appear over the shared content")
		}
		if a.pending != "" {
			t.Error("no code must be left valid")
		}
	})
}

func TestRandomPIN(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := randomPIN()
		if err != nil {
			t.Fatalf("generation failed: %v", err)
		}
		if len(p) != 4 {
			t.Fatalf("want a four-digit code, got %q", p)
		}
		seen[p] = true
	}
	// This is not a randomness test, but a generator stuck on one value would
	// make the access control purely ornamental.
	if len(seen) < 10 {
		t.Errorf("only %d distinct codes out of 50: suspicious generator", len(seen))
	}
}

func TestParseHello(t *testing.T) {
	t.Run("code and token", func(t *testing.T) {
		pin, tok, ok := parseHello(MsgHello + " 4821 abc123")
		if !ok || pin != "4821" || tok != "abc123" {
			t.Errorf("want 4821/abc123, got %q/%q (ok=%v)", pin, tok, ok)
		}
	})

	t.Run("another message is not an introduction", func(t *testing.T) {
		if _, _, ok := parseHello(MsgForce); ok {
			t.Error("a different message must not be read as an introduction")
		}
	})
}

func TestSplitToken(t *testing.T) {
	// The token tells sessions apart: without it a notice meant for an earlier run
	// on the same machine would stop the new one.
	if msg, tok := SplitToken(MsgBye + " abc123"); msg != MsgBye || tok != "abc123" {
		t.Errorf("want %q/abc123, got %q/%q", MsgBye, msg, tok)
	}
	if msg, tok := SplitToken(MsgBye); msg != MsgBye || tok != "" {
		t.Errorf("without a token: want %q/empty, got %q/%q", MsgBye, msg, tok)
	}
}

func TestStopIgnoresOtherSession(t *testing.T) {
	// The case that broke changing resolution from the extension: the receiver
	// says goodbye to the session that has just ended, and the goodbye reaches the
	// one that has just started on the same port.
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := senderStoppedBy(t, ctx, port)
	SendBye(net.IPv4(127, 0, 0, 1), port, "somebody-elses-token")

	select {
	case <-stopped:
		t.Fatal("stopped by a notice meant for another session")
	case <-time.After(400 * time.Millisecond):
	}
}

func TestControlSurvivesBusyPort(t *testing.T) {
	// Arbitration is a convenience: if the port is taken we give it up rather
	// than block the transmission.
	port := freePort(t)
	busy, err := net.ListenUDP("udp", &net.UDPAddr{Port: SendControlPort(port)})
	if err != nil {
		t.Skipf("control port could not be occupied: %v", err)
	}
	defer busy.Close()

	done := make(chan struct{})
	go func() {
		WaitForStop(context.Background(), port, testToken, func(string) { t.Error("nothing should have been stopped") })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForStop did not give up on a busy port")
	}
}
