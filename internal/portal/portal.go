package portal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest      = "org.freedesktop.portal.Desktop"
	portalPath      = "/org/freedesktop/portal/desktop"
	screenCastIface = "org.freedesktop.portal.ScreenCast"
	requestIface    = "org.freedesktop.portal.Request"
)

type portal struct {
	conn    *dbus.Conn
	signals chan *dbus.Signal
	session dbus.ObjectPath
}

// request invokes a portal method and waits for the matching Response signal.
// Portal calls are all asynchronous: they return the path of a Request, and the
// outcome arrives later as a signal on that path.
func (p *portal) request(ctx context.Context, method string, args ...interface{}) (map[string]dbus.Variant, error) {
	obj := p.conn.Object(portalDest, portalPath)

	var reqPath dbus.ObjectPath
	if err := obj.CallWithContext(ctx, screenCastIface+"."+method, 0, args...).Store(&reqPath); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sig, ok := <-p.signals:
			if !ok {
				return nil, errors.New("D-Bus connection closed")
			}
			if sig.Name != requestIface+".Response" || sig.Path != reqPath {
				continue
			}
			if len(sig.Body) < 2 {
				return nil, fmt.Errorf("%s: malformed response", method)
			}
			code, _ := sig.Body[0].(uint32)
			if code != 0 {
				return nil, fmt.Errorf("%s: sharing cancelled or refused (code %d)", method, code)
			}
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			return results, nil
		}
	}
}

// ScreenCast is what the portal hands over in order to capture the chosen
// screen. Width and Height are 0 if the portal did not declare a size.
type ScreenCast struct {
	NodeID uint32
	Width  int
	Height int

	// Kind is "screen" or "window": the only thing the portal says about *what*
	// it is handing over, and the only way the sender can tell whether it is
	// showing the whole desktop or a single window.
	Kind  string
	FD    *os.File
	Close func()
}

// describeProps renders a stream's properties readable, in a stable order
// because the D-Bus map has none.
func describeProps(props map[string]dbus.Variant) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, props[k].Value()))
	}
	return strings.Join(parts, " ")
}

// Masks for SelectSources: 1 screens, 2 windows.
const (
	sourceScreens = uint32(1)
	sourceWindows = uint32(2)
	sourceAny     = sourceScreens | sourceWindows
)

// ParseSourceTypes turns the value of --source into the portal's mask.
func ParseSourceTypes(s string) (uint32, error) {
	switch s {
	case "", "auto":
		return sourceAny, nil
	case "screen":
		return sourceScreens, nil
	case "window":
		return sourceWindows, nil
	}
	return 0, fmt.Errorf("unknown source %q: use auto, screen or window", s)
}

// StartScreenCast opens a session with the portal and returns the PipeWire node
// to capture plus the descriptor to the PipeWire daemon. Close must be called
// when transmission ends, or GNOME's screen-sharing indicator stays lit for
// nothing.
func StartScreenCast(ctx context.Context, sourceTypes uint32) (*ScreenCast, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	fail := func(err error) (*ScreenCast, error) {
		conn.Close()
		return nil, err
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return fail(fmt.Errorf("registering signals: %w", err))
	}

	p := &portal{conn: conn, signals: make(chan *dbus.Signal, 16)}
	conn.Signal(p.signals)

	res, err := p.request(ctx, "CreateSession", map[string]dbus.Variant{
		"handle_token":         dbus.MakeVariant("gocast_create"),
		"session_handle_token": dbus.MakeVariant("gocast_session"),
	})
	if err != nil {
		return fail(err)
	}
	v, ok := res["session_handle"]
	if !ok {
		return fail(errors.New("the portal returned no session_handle"))
	}
	var sessionHandle string
	if err := v.Store(&sessionHandle); err != nil {
		return fail(fmt.Errorf("session_handle: %w", err))
	}
	p.session = dbus.ObjectPath(sessionHandle)

	// types is a bit mask: 1 whole screens, 2 single windows. Asking for both
	// makes GNOME's picker offer both, but the stream still comes out as large as
	// the monitor: a window arrives painted at the top left with the rest black.
	// Asking for windows only can change how the portal sets the stream up, which
	// is why the type is selectable.
	// cursor_mode=2 paints the cursor into the stream.
	if _, err := p.request(ctx, "SelectSources", p.session, map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant("gocast_select"),
		"types":        dbus.MakeVariant(sourceTypes),
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(uint32(2)),
	}); err != nil {
		return fail(err)
	}

	// This is where the screen picker appears: Start does not return until the
	// user has confirmed.
	res, err = p.request(ctx, "Start", p.session, "", map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant("gocast_start"),
	})
	if err != nil {
		return fail(err)
	}

	var streams []struct {
		NodeID uint32
		Props  map[string]dbus.Variant
	}
	sv, ok := res["streams"]
	if !ok {
		return fail(errors.New("the portal returned no streams"))
	}
	if err := sv.Store(&streams); err != nil {
		return fail(fmt.Errorf("streams: %w", err))
	}
	if len(streams) == 0 {
		return fail(errors.New("no screen selected"))
	}

	// The portal describes each stream with a set of properties — source_type
	// (1 screen, 2 window), size, position, mapping_id — and those are the only
	// authoritative description of what we are capturing. All of them get logged:
	// inferring the geometry from the caps alone has already led us astray.
	for i, s := range streams {
		log.Printf("portal, stream %d (node %d): %s", i, s.NodeID, describeProps(s.Props))
	}

	// The portal declares the source size. Taking it from here spares videoscale
	// from inferring it: pipewiresrc's caps carry no pixel-aspect-ratio and have
	// framerate=0/1, and under those conditions computing the height from the
	// width alone overflows.
	var size struct{ W, H int32 }
	if v, ok := streams[0].Props["size"]; ok {
		if err := v.Store(&size); err != nil {
			size.W, size.H = 0, 0
		}
	}

	// The portal says whether it is handing over a screen or a window. It is
	// little, but it is the only thing that qualifies what is being shared — and
	// whoever transmits deserves to know, given the receiver may be in another
	// room.
	var srcType uint32
	if v, ok := streams[0].Props["source_type"]; ok {
		_ = v.Store(&srcType)
	}

	var ufd dbus.UnixFD
	if err := conn.Object(portalDest, portalPath).CallWithContext(
		ctx, screenCastIface+".OpenPipeWireRemote", 0, p.session, map[string]dbus.Variant{},
	).Store(&ufd); err != nil {
		return fail(fmt.Errorf("OpenPipeWireRemote: %w", err))
	}
	fd := os.NewFile(uintptr(ufd), "pipewire")

	return &ScreenCast{
		NodeID: streams[0].NodeID,
		Width:  int(size.W),
		Height: int(size.H),
		Kind:   sourceKind(srcType),
		FD:     fd,
		Close: func() {
			conn.Object(portalDest, p.session).Call("org.freedesktop.portal.Session.Close", 0)
			fd.Close()
			conn.Close()
		},
	}, nil
}

// sourceKind translates the portal's mask: 1 screens, 2 windows.
func sourceKind(t uint32) string {
	switch t {
	case sourceScreens:
		return "screen"
	case sourceWindows:
		return "window"
	default:
		return "unknown"
	}
}

// IsWayland reports whether the graphical session is Wayland, and therefore
// whether screen capture must go through the portal rather than X11.
func IsWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}
