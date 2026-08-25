package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPort is the port the receiver accepts the stream on; the two after it
// carry the control channel (see recvControlPort and SendControlPort).
const DefaultPort = 5000

// The control channel between sender and receiver.
//
// It serves two purposes the media connection does not cover: telling a sender
// that nobody is watching it any more, and arbitrating between several senders
// that want to transmit at once — without an arbiter their streams would mix
// into the same pipeline and produce garbage.
//
// By convention the ports are the two after the data port, so nothing has to be
// negotiated. Receiver and sender use one each because they may end up on the
// same machine, as happens when testing locally.
const (
	MsgBye   = "GOCAST-BYE"   // receiver -> sender: I have closed
	MsgBusy  = "GOCAST-BUSY"  // receiver -> sender: somebody else is already on
	MsgDeny  = "GOCAST-DENY"  // receiver -> sender: code missing or wrong
	MsgForce = "GOCAST-FORCE" // sender -> receiver: I am taking over
	MsgHello = "GOCAST-HELLO" // sender -> receiver: introducing myself, here is the code

	// Pairing: the code is shown on the receiver's own screen, so that only
	// whoever is standing in front of it can read it.
	MsgPair   = "GOCAST-PAIR"    // sender -> receiver: show me the code
	MsgPaired = "GOCAST-PAIRED"  // receiver -> sender: code accepted
	MsgNoPair = "GOCAST-NOPAIR"  // receiver -> sender: not now, I am transmitting
	MsgShown  = "GOCAST-SHOWING" // receiver -> sender: code is on screen, type it in
)

func recvControlPort(dataPort int) int { return dataPort + 1 }
func SendControlPort(dataPort int) int { return dataPort + 2 }

// The control channel belongs to the machine, not to a session: its port is
// derived from the data port, so two consecutive runs of the sender on the same
// machine listen on the same port.
//
// Without a session identity, a notice meant for the sender that has just ended
// gets delivered to the one that has just started, which stops believing it was
// meant for it. This happens on every quick restart — changing resolution from
// the extension is exactly that — and from outside it looks as if transmission
// no longer starts.
//
// So every sender generates a token, declares it when introducing itself, and
// ignores messages that do not carry it.
func SessionToken() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "0"
	}
	return hex.EncodeToString(buf)
}

// SplitToken separates a message from the token that accompanies it.
func SplitToken(text string) (msg, token string) {
	if i := strings.LastIndex(text, " "); i >= 0 {
		return text[:i], text[i+1:]
	}
	return text, ""
}

// notify sends a one-shot message to a control endpoint. A lost notice is not
// serious: it degrades to the earlier behaviour, which is stopping things by
// hand.
func notify(to net.IP, port int, msg string) {
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: to, Port: port})
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte(msg))
}

// Every message towards a sender carries the token of the session it is meant
// for, or it ends up stopping the wrong one.
func SendBye(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgBye+" "+token)
}

func SendBusy(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgBusy+" "+token)
}

func SendDeny(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgDeny+" "+token)
}

func SendForce(to net.IP, dataPort int) {
	notify(to, recvControlPort(dataPort), MsgForce)
}

func SendPairRequest(to net.IP, dataPort int) { notify(to, recvControlPort(dataPort), MsgPair) }
func sendPaired(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgPaired+" "+token)
}

func sendNoPair(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgNoPair+" "+token)
}

func sendShowing(to net.IP, dataPort int, token string) {
	notify(to, SendControlPort(dataPort), MsgShown+" "+token)
}

// SendHello introduces the sender: access code and session token.
func SendHello(to net.IP, dataPort int, pin, token string) {
	notify(to, recvControlPort(dataPort), fmt.Sprintf("%s %s %s", MsgHello, pin, token))
}

// parseHello extracts code and token from an introduction.
//
// The code may be empty — an open receiver asks for none — so the fields are
// counted rather than split and hoped for.
func parseHello(text string) (pin, token string, ok bool) {
	if !strings.HasPrefix(text, MsgHello) {
		return "", "", false
	}
	f := strings.Fields(strings.TrimPrefix(text, MsgHello))
	switch len(f) {
	case 0:
		return "", "", true
	case 1:
		return f[0], "", true
	default:
		return f[0], f[1], true
	}
}

// randomPIN generates a four-digit code.
//
// From crypto/rand rather than math/rand: this is an access control, however
// light, and a predictable generator would make it ornamental.
func randomPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

// ListenControl delivers the messages received on a control port.
//
// It closes the channel when it stops listening — port taken or context done —
// so that whoever reads it notices, instead of waiting for messages that will
// never come.
//
// If the port is unavailable it gives up: arbitration is a convenience, not a
// requirement, and transmitting anyway is worth more.
func ListenControl(ctx context.Context, port string, portNum int, out chan<- Msg) {
	defer close(out)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: portNum})
	if err != nil {
		log.Printf("%s control channel unavailable: %v", port, err)
		return
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 64)
	for ctx.Err() == nil {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := strings.TrimSpace(string(buf[:n]))
		select {
		case out <- Msg{Text: msg, From: from.IP}:
		case <-ctx.Done():
			return
		}
	}
}

type Msg struct {
	Text string
	From net.IP
}

// Arbiter holds the receiver's arbitration state: who has supplied the access
// code and who has asked to take over.
//
// Control messages arrive even while nobody is transmitting — a sender
// introduces itself before it starts — so they must be collected continuously,
// not only during a session.
type Arbiter struct {
	pairing bool          // when false the receiver is open to anybody
	window  time.Duration // how long the code stays on screen

	// show puts the code on the receiver's screen, hide takes it away.
	show func(code string)
	hide func()

	mu       sync.Mutex
	accepted map[string]bool   // codes already paired, remembered across restarts
	known    map[string]bool   // addresses authorised during this run
	tokens   map[string]string // last session token announced by each address
	pending  string            // code currently on screen, "" if none
	until    time.Time         // how long it stays valid

	Busy  atomic.Bool // a transmission is in progress
	Force chan net.IP
}

// TokenFor returns the token declared by an address's latest introduction, used
// to address messages to the right session.
func (a *Arbiter) TokenFor(ip net.IP) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens[ip.String()]
}

func (a *Arbiter) rememberToken(ip net.IP, token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	a.tokens[ip.String()] = token
	a.mu.Unlock()
}

func NewArbiter(ctx context.Context, dataPort int, pairing bool, window time.Duration,
	show func(string), hide func()) *Arbiter {

	a := &Arbiter{
		pairing:  pairing,
		window:   window,
		show:     show,
		hide:     hide,
		accepted: loadPaired(),
		known:    make(map[string]bool),
		tokens:   make(map[string]string),
		Force:    make(chan net.IP, 4),
	}

	msgs := make(chan Msg, 8)
	go ListenControl(ctx, "receiver", recvControlPort(dataPort), msgs)
	go a.run(ctx, msgs, dataPort)
	return a
}

func (a *Arbiter) run(ctx context.Context, msgs <-chan Msg, dataPort int) {
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			switch {
			case m.Text == MsgForce:
				select {
				case a.Force <- m.From:
				default: // repeated requests: the first one is enough
				}
			case m.Text == MsgPair:
				a.startPairing(ctx, m.From, dataPort)
			default:
				if pin, token, isHello := parseHello(m.Text); isHello {
					// The token must be remembered before replying: it is what
					// addresses the reply to the right session.
					a.rememberToken(m.From, token)
					a.greet(m.From, pin, dataPort)
				}
			}
		}
	}
}

// startPairing puts a fresh code on the screen.
//
// Refused during a transmission, and not out of caution: the screen is showing
// the shared content, so the code would land in front of whoever is watching —
// which is exactly who pairing is meant to keep out.
func (a *Arbiter) startPairing(ctx context.Context, from net.IP, dataPort int) {
	if !a.pairing {
		return // open receiver: there is nothing to pair
	}
	if a.Busy.Load() {
		log.Printf("pairing request from %s refused: transmission in progress", from)
		sendNoPair(from, dataPort, a.TokenFor(from))
		return
	}

	code, err := randomPIN()
	if err != nil {
		log.Printf("could not generate the code: %v", err)
		return
	}

	a.mu.Lock()
	a.pending = code
	a.until = time.Now().Add(a.window)
	a.mu.Unlock()

	log.Printf("pairing requested by %s — code on screen: %s", from, code)
	if a.show != nil {
		a.show(code)
	}
	sendShowing(from, dataPort, a.TokenFor(from))

	// The code must not stay valid for long: it is the window in which whoever
	// is in front of the screen copies it down, not a password.
	go func() {
		select {
		case <-ctx.Done():
			return // shutting down: this is not an expiry
		case <-time.After(a.window):
		}
		a.mu.Lock()
		expired := a.pending == code
		if expired {
			a.pending = ""
		}
		a.mu.Unlock()
		if expired {
			log.Print("pairing expired without being completed")
			if a.hide != nil {
				a.hide()
			}
		}
	}()
}

func (a *Arbiter) greet(from net.IP, pin string, dataPort int) {
	if !a.pairing {
		return // open to anybody: there is nothing to check
	}

	a.mu.Lock()
	okKnown := a.accepted[pin]
	okPending := pin != "" && pin == a.pending && time.Now().Before(a.until)
	if okPending {
		a.accepted[pin] = true
		a.pending = ""
	}
	if okKnown || okPending {
		a.known[from.String()] = true
	}
	accepted := make(map[string]bool, len(a.accepted))
	for k := range a.accepted {
		accepted[k] = true
	}
	a.mu.Unlock()

	switch {
	case okPending:
		log.Printf("%s paired", from)
		if a.hide != nil {
			a.hide()
		}
		if err := savePaired(accepted); err != nil {
			log.Printf("pairing not stored: %v", err)
		}
		sendPaired(from, dataPort, a.TokenFor(from))
	case okKnown:
		log.Printf("%s recognised", from)
		sendPaired(from, dataPort, a.TokenFor(from))
	default:
		log.Printf("invalid code from %s", from)
		SendDeny(from, dataPort, a.TokenFor(from))
	}
}

// Allowed reports whether an address may transmit. With pairing off anybody on
// the network may, which is the default behaviour.
func (a *Arbiter) Allowed(ip net.IP) bool {
	if a == nil || !a.pairing {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.known[ip.String()]
}

// Why the sender stopped, reported back to whoever started it.
const (
	stopClosed = "closed" // the receiver closed
	StopBusy   = "busy"   // another sender is already on
	StopDenied = "denied" // code missing or wrong
)

// WaitForStop stops the sender when the receiver closes or refuses it, saying
// why: a stored code has to be forgotten if it turns out to be invalid, and
// without the reason the caller could not tell the cases apart.
func WaitForStop(ctx context.Context, dataPort int, token string, stop func(reason string)) {
	msgs := make(chan Msg, 4)
	go ListenControl(ctx, "sender", SendControlPort(dataPort), msgs)

	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return // no control channel: carry on without it
			}

			// The control port belongs to the machine, not to this run: a notice
			// meant for the previous session still arrives here, and without a
			// token it would stop the wrong one. This happens on every quick
			// restart — changing resolution from the extension is exactly that.
			text, got := SplitToken(m.Text)
			if got != token {
				continue
			}

			switch text {
			case MsgBye:
				log.Print("the receiver closed: stopping the transmission")
				stop(stopClosed)
				return
			case MsgBusy:
				log.Printf("the receiver is already busy with another sender. "+
					"To take over: --force (host %s)", m.From)
				stop(StopBusy)
				return
			case MsgDeny:
				log.Printf("receiver %s requires an access code, "+
					"or the one supplied is invalid: use --pin CODE", m.From)
				stop(StopDenied)
				return
			}
		}
	}
}

// freeUDPPort finds a free port. The video stream now travels over TCP, but the
// control channel — goodbyes, refusals, takeovers — stays on datagrams, and
// that is where this is needed.
func freeUDPPort() (int, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return 0, fmt.Errorf("no local port available: %w", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port, nil
}
