package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gocast/internal/control"
	"gocast/internal/media"
	"gocast/internal/version"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const serviceType = "_gocast._udp"

// txtPairing is the key with which the receiver declares, in its mDNS record,
// whether it requires pairing.
//
// Declaring it up front spares the sender from discovering it the hard way: a
// client can offer pairing when it is needed and go straight through when it is
// not, instead of trying to transmit and being turned away.
const txtPairing = "pairing="

// txtID is the receiver's stable identity, independent of its address.
//
// Pairing must be tied to this rather than to the IP: under DHCP the address
// changes, and tying the stored code to it would force the sender to pair again
// although nothing of substance changed.
const txtID = "id="

// txtMaxWidth is the widest picture the receiver can decode.
//
// Declaring it is its job: it is the one that knows its own decoder, while the
// sender has no way to guess. A Raspberry Pi 3 stops at 1080p, and without this
// figure somebody transmitting from an ultrawide would send it 3440 pixels of
// width it cannot decode.
const txtMaxWidth = "maxw="

// txtMaxHeight is the height of the receiver's screen.
//
// It goes with the width and changes the nature of the figure: with both, the
// sender no longer has a ceiling to respect but a frame to fill exactly. It is
// there for kmssink, which draws by setting a display mode and refuses any size
// that is not one of the monitor's own.
const txtMaxHeight = "maxh="

// txtModes lists the other resolutions the receiver's screen accepts, beyond
// the preferred one carried by maxw/maxh.
//
// Without this a sender asking for a lower resolution has nowhere to go: the
// receiver draws by setting a display mode and refuses any size that is not one
// of the screen's own, so the sender cannot simply pick a smaller number. With
// the list it can choose the largest mode that fits what was asked for.
//
// Kept short on purpose: a TXT record is limited to 255 bytes per string, and
// the small modes at the bottom of the list are of no use to anybody.
const txtModes = "modes="

// txtVersion is what the receiver is running.
//
// The two halves live on different machines and are updated at different times.
// A sender talking to an older receiver — or the other way round — fails in ways
// that look like anything but a version gap: a flag that is ignored, a
// resolution that never changes, a black screen. Announcing it costs a dozen
// bytes and turns an hour of diagnosis into one line.
const txtVersion = "ver="

// maxAnnouncedModes is how many are worth announcing.
const maxAnnouncedModes = 8

type Receiver struct {
	Name string
	Host string
	Port int
	ID   string

	// Pairing: the receiver requires pairing.
	// Paired: this PC has already paired with it — filled in by whoever holds the
	// stored codes, see control.RecallPin. Discovery leaves it false: it reports
	// what it heard announced, not what this computer remembers.
	//
	// Together they tell a client whether it can transmit right away, whether it
	// has to pair first, or whether there is nothing to be done — without having
	// to find out by being turned away.
	Pairing bool
	Paired  bool

	// MaxWidth: widest decodable picture, 0 if the receiver sets no limit.
	MaxWidth int

	// MaxHeight: the height of the receiver's screen, 0 if it did not declare it.
	// When present, together with MaxWidth it describes an exact frame to fill.
	MaxHeight int

	// Modes: the other resolutions the screen accepts, preferred first. Empty
	// for a receiver that does not announce them.
	Modes []media.Rect

	// Version of the receiver, empty for one too old to announce it.
	Version string
}

func Announce(name string, port int, pairing bool, id string, maxWidth, maxHeight int,
	modes []media.Rect) (*zeroconf.Server, error) {
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		name = h
		// The announced name has to tell instances apart: on a non-standard port
		// there is almost certainly a second receiver on the same machine, and two
		// records with the same name are indistinguishable to whoever is choosing
		// where to transmit.
		if port != control.DefaultPort {
			name = fmt.Sprintf("%s:%d", h, port)
		}
	}
	flag := "0"
	if pairing {
		flag = "1"
	}
	txt := []string{
		txtPairing + flag,
		txtID + id,
		txtMaxWidth + strconv.Itoa(maxWidth),
		txtMaxHeight + strconv.Itoa(maxHeight),
		txtVersion + version.String(),
	}
	if list := encodeModes(modes); list != "" {
		txt = append(txt, txtModes+list)
	}
	return zeroconf.Register(name, serviceType, "local.", port, txt, nil)
}

// encodeModes renders the modes as "1920x1080,1600x900,1280x720".
//
// Only the ones shaped like the screen, widest first. A television lists plenty
// of modes inherited from the monitor world — 1280x1024 is 5:4, 1280x800 is
// 16:10 — and picking one of those to honour a request for "1280 wide" would
// letterbox a 16:9 desktop into the wrong shape. Filtering by aspect ratio also
// keeps the useful sizes from being pushed out of the record by the odd ones.
func encodeModes(modes []media.Rect) string {
	if len(modes) == 0 {
		return ""
	}
	shape := ratio(modes[0])

	var kept []media.Rect
	for _, m := range modes {
		if m.Empty() || !sameShape(ratio(m), shape) {
			continue
		}
		kept = append(kept, m)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].W > kept[j].W })

	var parts []string
	for _, m := range kept {
		if len(parts) >= maxAnnouncedModes {
			break
		}
		parts = append(parts, fmt.Sprintf("%dx%d", m.W, m.H))
	}
	return strings.Join(parts, ",")
}

func ratio(r media.Rect) float64 {
	if r.H == 0 {
		return 0
	}
	return float64(r.W) / float64(r.H)
}

// sameShape allows a couple of per cent: 1366x768 is 16:9 in intent and
// 1.779 in arithmetic, and excluding it would drop a mode every screen has.
func sameShape(a, b float64) bool {
	if b == 0 {
		return false
	}
	d := a/b - 1
	return d > -0.03 && d < 0.03
}

// decodeModes reads back what encodeModes wrote, skipping anything malformed:
// an unreadable entry costs one resolution, a refusal to parse would cost the
// whole announcement.
func decodeModes(s string) []media.Rect {
	var out []media.Rect
	for _, part := range strings.Split(s, ",") {
		w, h, ok := strings.Cut(strings.TrimSpace(part), "x")
		if !ok {
			continue
		}
		W, err1 := strconv.Atoi(w)
		H, err2 := strconv.Atoi(h)
		if err1 != nil || err2 != nil || W <= 0 || H <= 0 {
			continue
		}
		out = append(out, media.Rect{W: W, H: H})
	}
	return out
}

func browse(ctx context.Context, timeout time.Duration) ([]Receiver, error) {
	res, err := zeroconf.NewResolver()
	if err != nil {
		return nil, err
	}

	entries := make(chan *zeroconf.ServiceEntry, 8)
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := res.Browse(bctx, serviceType, "local.", entries); err != nil {
		return nil, err
	}

	// The channel is closed when bctx expires: the range ends on its own.
	//
	// The dedup key is address *and* port: deduplicating on the address alone
	// collapses several receivers on one machine into a single entry, and which
	// one survives depends on the order in which they answer. A sender then ends
	// up transmitting to a receiver other than the one being watched, with
	// nothing to signal it.
	var out []Receiver
	seen := make(map[string]bool)
	for e := range entries {
		var host string
		switch {
		case len(e.AddrIPv4) > 0:
			host = e.AddrIPv4[0].String()
		case len(e.AddrIPv6) > 0:
			host = e.AddrIPv6[0].String()
		}
		if host == "" {
			continue
		}
		key := net.JoinHostPort(host, strconv.Itoa(e.Port))
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, receiverFrom(e.Instance, host, e.Port, e.Text))
	}
	return out, nil
}

// receiverFrom reads an announcement into a receiver.
//
// Everything here comes off the wire except one field: whether we have paired.
// That one is ours to answer — the receiver announces that it wants a code, and
// only this machine knows whether it holds one. Left unanswered, which it was,
// a receiver already paired went on being reported as needing pairing: the
// extension kept offering "pair first", and the sender refused to transmit to
// it without the code being typed out again on the command line.
func receiverFrom(name, host string, port int, txt []string) Receiver {
	r := Receiver{Name: name, Host: host, Port: port}
	for _, t := range txt {
		switch {
		case strings.HasPrefix(t, txtPairing):
			r.Pairing = strings.TrimPrefix(t, txtPairing) == "1"
		case strings.HasPrefix(t, txtID):
			r.ID = strings.TrimPrefix(t, txtID)
		case strings.HasPrefix(t, txtMaxHeight):
			r.MaxHeight, _ = strconv.Atoi(strings.TrimPrefix(t, txtMaxHeight))
		case strings.HasPrefix(t, txtVersion):
			r.Version = strings.TrimPrefix(t, txtVersion)
		case strings.HasPrefix(t, txtModes):
			r.Modes = decodeModes(strings.TrimPrefix(t, txtModes))
		case strings.HasPrefix(t, txtMaxWidth):
			r.MaxWidth, _ = strconv.Atoi(strings.TrimPrefix(t, txtMaxWidth))
		}
	}
	// The identity is read before this line is reached, and it is what the code
	// is filed under: the order matters.
	r.Paired = r.paired()
	return r
}

// paired reports whether this machine holds a code for the receiver.
func (r Receiver) paired() bool { return control.RecallPin(r.Key()) != "" }

// Key is the name under which the sender remembers this receiver's code.
//
// The declared identity when there is one, the address otherwise: an older
// receiver, or one reached by IP without going through discovery, has none, and
// in that case we fall back to the earlier behaviour.
func (r Receiver) Key() string {
	if r.ID != "" {
		return r.ID
	}
	return r.Host
}

// Lookup finds the receiver at a given address among the announcements, to read
// off what it declares: resolution, pairing, identity.
//
// It insists rather than trying once. The announcement appears when the
// receiver is ready, and whoever transmits often launches at the same moment or
// a touch earlier: with a single three-second attempt the frame is lost over a
// question of startup order, and the sender carries on transmitting the
// source's own resolution — which a receiver on HDMI cannot show. The symptom
// is a black screen with no error anywhere, and it cost more than an hour of
// diagnosis.
func Lookup(ctx context.Context, host string, port int, wait time.Duration) Receiver {
	deadline := time.Now().Add(lookupPatience)
	for attempt := 1; ; attempt++ {
		rs, err := browse(ctx, wait)
		if err == nil {
			for _, r := range rs {
				if r.Host == host && r.Port == port {
					return r
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return Receiver{Host: host, Port: port}
		}
		log.Printf("receiver %s has not announced itself yet, retrying (%d)", host, attempt)
	}
}

// How long to insist before giving up on the announcement.
const lookupPatience = 12 * time.Second

func List(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	wait := fs.Duration("wait", 3*time.Second, "how long to search")
	asJSON := fs.Bool("json", false, "JSON output (used by the GNOME extension)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rs, err := browse(ctx, *wait)
	if err != nil {
		return err
	}

	// The shell extension reads this: with --json a valid array always comes out,
	// empty included, so the parser on the other side has no special cases.
	if *asJSON {
		if rs == nil {
			rs = []Receiver{}
		}
		out, err := json.Marshal(rs)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	if len(rs) == 0 {
		fmt.Println("no receiver found")
		return nil
	}
	for _, r := range rs {
		fmt.Printf("%-24s %s:%-6d %-26s %-22s %s\n",
			r.Name, r.Host, r.Port, pairingState(r), widthLimit(r), versionOf(r))
	}
	return nil
}

// versionOf marks a receiver whose version differs from ours: the mismatch is
// worth seeing before it turns into a feature that mysteriously does nothing.
func versionOf(r Receiver) string {
	switch {
	case r.Version == "":
		return "(version unknown)"
	case r.Version != version.String():
		return "v" + r.Version + " (we are v" + version.String() + ")"
	default:
		return "v" + r.Version
	}
}

func pairingState(r Receiver) string {
	switch {
	case !r.Pairing:
		return "open"
	case r.Paired:
		return "paired"
	default:
		return "pairing needed (gocast pair)"
	}
}

func widthLimit(r Receiver) string {
	if r.MaxWidth > 0 && r.MaxHeight > 0 {
		return fmt.Sprintf("screen %dx%d", r.MaxWidth, r.MaxHeight)
	}
	if r.MaxWidth <= 0 {
		return "no width limit"
	}
	return fmt.Sprintf("max %dpx", r.MaxWidth)
}

func FindReceiver(ctx context.Context, name string, wait time.Duration) (Receiver, error) {
	rs, err := browse(ctx, wait)
	if err != nil {
		return Receiver{}, err
	}
	if len(rs) == 0 {
		return Receiver{}, errors.New("no receiver found: run `gocast serve` on the TV, or pass --host")
	}
	if name == "" {
		return rs[0], nil
	}
	for _, r := range rs {
		if strings.EqualFold(r.Name, name) {
			return r, nil
		}
	}
	return Receiver{}, fmt.Errorf("receiver %q not found", name)
}
