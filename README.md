<p align="center">
  <img src="misc/gocast.png" alt="gocast" width="620">
</p>

<p align="center">
  <b>Share your screen on a TV over the wire</b><br>
  from a GNOME/Wayland desktop to a Raspberry Pi plugged into the HDMI port
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.21+">
  <img src="https://img.shields.io/badge/platform-Linux%20%C2%B7%20Wayland-333" alt="Linux / Wayland">
  <img src="https://img.shields.io/badge/receiver-Raspberry%20Pi-C51A4A?logo=raspberrypi&logoColor=white" alt="Raspberry Pi">
  <img src="https://img.shields.io/badge/licence-MIT-blue" alt="MIT">
</p>

---

No Wi-Fi Direct, no cloud, no browser. The PC captures the screen through the
desktop portal, encodes it with x264 and pushes an MPEG-TS over TCP; the Pi
decodes it in hardware and paints it straight onto the display.

```
┌─────────────────────────┐                      ┌──────────────────────────┐
│ PC — GNOME/Wayland      │                      │ Raspberry Pi — console   │
│                         │      TCP :5000       │                          │
│ portal ▸ x264 ▸ mpegts ─┼──────────────────────┼▸ tsdemux ▸ v4l2 ▸ kmssink│
│                         │   mDNS + control     │                          │
│ GNOME extension         │◂────── :5001/5002 ──▸│ arbiter, pairing         │
└─────────────────────────┘                      └──────────────────────────┘
```

## Why not Miracast

Miracast **is** Wi-Fi Direct. There is no wired variant: the two devices form a
peer-to-peer radio link, and the protocol has no notion of running over
Ethernet. `gnome-network-displays` speaks Miracast (Wi-Fi) and Chromecast (LAN
via a Google-specific protocol), so neither gives you plain screen sharing over
a cable.

gocast does the same job over the network you already have, at the quality a
cable allows.

## Features

- **Automatic discovery** over mDNS — receivers announce themselves, senders
  list them.
- **Hardware decoding** on the Pi (`v4l2h264dec`), with automatic fallback when
  a decoder turns out not to cope.
- **The receiver dictates the resolution**: it announces the mode of the screen
  it is attached to, and the sender letterboxes into it.
- **Single window or whole screen**, with automatic cropping of the useful
  content.
- **One sender at a time**, with an explicit takeover.
- **Pairing by a code shown on the receiver's own screen** — only somebody in
  front of the TV can read it.
- **Audio over HDMI**, in a process of its own so that a sound fault cannot take
  the picture down.
- **GNOME Shell extension**: pick a receiver from the top bar.

## Requirements

### On the PC — Ubuntu / Debian

```console
$ sudo apt install golang-go \
    gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad gstreamer1.0-plugins-ugly gstreamer1.0-libav \
    gstreamer1.0-pipewire
```

`plugins-ugly` is not optional despite the name: **`x264enc` lives there**, and
without it nothing gets encoded. `gstreamer1.0-pipewire` provides `pipewiresrc`,
which is the only way to capture the screen under Wayland, and `libav` provides
the AAC encoder for the audio.

You also need **GNOME on Wayland** — capture goes through
`xdg-desktop-portal` — and **Go 1.21+** to build.

### On the receiver — Raspberry Pi OS

```console
$ sudo apt install \
    gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad gstreamer1.0-libav gstreamer1.0-alsa
```

`plugins-bad` provides `kmssink`, which draws straight onto the HDMI output;
`gstreamer1.0-alsa` provides `alsasink` and is a separate package on Debian 13,
while on Debian 11 the same element sits inside `plugins-base`. No encoder is
needed here, so `plugins-ugly` can be left out.

Two further requirements, and both have bitten:

- **Bookworm or newer.** Bullseye ships GStreamer 1.18, whose V4L2 decoder does
  not work at all — see [Known traps](#known-traps).
- **A console session, no desktop.** A running compositor owns the display and
  `kmssink` cannot take it.

## Build

```console
$ make build          # binary for the PC
$ make test           # go vet + go test + build every package
$ make install        # build and install into /usr/local/bin (asks for sudo)

$ make pi32           # binary for a 32-bit Raspberry Pi
$ make pi64           # for a 64-bit one
```

## Install

### On the receiver

Copy the binary over and install it as a service:

```console
$ scp gocast-armv7 pi@raspberry:/tmp/
```

then, on the Pi:

```console
$ sudo install -m755 /tmp/gocast-armv7 /usr/local/bin/gocast
$ sudo tee /etc/systemd/system/gocast.service >/dev/null <<'EOF'
[Unit]
Description=gocast receiver
After=network-online.target sound.target

[Service]
User=root
ExecStart=/usr/local/bin/gocast serve
Restart=always
RestartSec=2
StandardOutput=append:/var/log/gocast.log
StandardError=append:/var/log/gocast.log

[Install]
WantedBy=multi-user.target
EOF
$ sudo systemctl enable --now gocast
```

> **`User=root` is not laziness.** Setting a video mode requires being DRM
> master, and the kernel only grants that to a session with a seat on the
> console. A receiver started over SSH has none: the sink exits successfully and
> draws into the void — no error, a frozen screen. This cost a night to find.

### On the PC

```console
$ make install
$ make install-extension    # optional: GNOME Shell extension
```

The extension is plain JavaScript, so there is nothing to compile. Under Wayland
it needs a reload:

```console
$ gnome-extensions disable gocast@local && gnome-extensions enable gocast@local
```

It runs `gocast` from `$PATH`, which is why the binary belongs in
`/usr/local/bin` — `gnome-shell` does not have `~/bin` on its path.

## Usage

```console
$ gocast list                          # who is on the network
raspberry   192.168.1.97:5000   open   screen 1920x1080

$ gocast send                          # to the first receiver found
$ gocast send --host 192.168.1.97      # to a specific one
$ gocast send --bitrate 6000 --fps 30  # tuning
```

On the receiver, when not running as a service:

```console
$ gocast serve
$ gocast serve --pairing               # require a pairing code
$ gocast serve --player ffmpeg         # for a lossy link, see below
```

Pairing, once per PC:

```console
$ gocast pair --host 192.168.1.97
Code shown on the receiver's screen. Type it here: 4821
Paired with raspberry. From now on you can transmit without retyping the code.
```

<details>
<summary><b>All the options</b></summary>

**`gocast send`**

| Flag | Default | |
|---|---|---|
| `--host` | mDNS search | receiver address |
| `--bitrate` | 12000 | video bitrate, kbit/s |
| `--fps` | 25 | maximum frames per second |
| `--keyint` | one per second | frames between keyframes |
| `--preset` | veryfast | x264 preset |
| `--crop` | auto | `auto`, `WIDTHxHEIGHT[+X+Y]` or `no` |
| `--source` | auto | what the picker offers: `auto`, `screen`, `window` |
| `--audio` | true | transmit the PC audio as well |
| `--force` | false | take over from another sender |
| `--pin` | | access code, if the receiver asks for one |
| `--stretch` | false | distort instead of letterboxing |

**`gocast serve`**

| Flag | Default | |
|---|---|---|
| `--port` | 5000 | TCP port |
| `--sink` | detected | GStreamer video sink |
| `--player` | auto | `auto`, `gstreamer`, `ffmpeg` |
| `--decoder` | detected | pin a decoder |
| `--pairing` | false | require a pairing code |
| `--max-width` / `--max-height` | from the screen | what to announce |
| `--audio-device` | detected HDMI | ALSA device |

</details>

## How it works

### Transport: TCP, not UDP

Screen sharing is usually built on RTP/UDP, on the grounds that a late frame is
worth less than a low-latency one. Towards a Raspberry Pi that reasoning breaks
down, measured:

| bitrate | continuity errors in 10 s |
|---|---|
| 2 Mbit/s | 0 |
| 5 Mbit/s | 11 |
| 11 Mbit/s | 19 |

The Pi's network goes through USB, and above two or three Mbit/s it drops
packets. Every lost packet wrecks a slice of a frame, and since keyframes are
large and travel in hundreds of consecutive packets, **not one intact keyframe
gets through**. Without a keyframe there is no picture to rebuild, while audio —
made of tiny packets — plays perfectly. The symptom is "audio only", with no
error anywhere.

Over TCP whatever is lost is retransmitted. Same content, same bitrate, a
flawless picture. It also simplifies the lifecycle: a connection closing *is* the
end of the transmission, stated by the protocol, instead of being guessed from
prolonged silence.

### The control channel

The video travels over TCP, but the negotiation around it — who may transmit,
who is already transmitting, when to stop — stays on UDP datagrams, on two ports
derived by convention from the data port:

| port | who listens | what arrives |
|---|---|---|
| 5000 | receiver | the stream itself, over TCP |
| 5001 | receiver | `HELLO`, `PAIR`, `FORCE` from senders |
| 5002 | sender | `BYE`, `BUSY`, `DENY`, `PAIRED`, `NOPAIR`, `SHOWING` |

Two ports rather than one because receiver and sender can end up on the same
machine, which is exactly what happens when testing locally.

The messages are plain text, one datagram each:

```
GOCAST-HELLO 4821 3fa9c2b7e104     sender ▸ receiver   access code, session token
GOCAST-PAIR                        sender ▸ receiver   show me the pairing code
GOCAST-FORCE                       sender ▸ receiver   I am taking over

GOCAST-BYE      3fa9c2b7e104       receiver ▸ sender   I have closed
GOCAST-BUSY     3fa9c2b7e104       receiver ▸ sender   somebody else is on
GOCAST-DENY     3fa9c2b7e104       receiver ▸ sender   code missing or wrong
GOCAST-SHOWING  3fa9c2b7e104       receiver ▸ sender   code is on screen
GOCAST-PAIRED   3fa9c2b7e104       receiver ▸ sender   code accepted
```

**Why UDP, when the stream is TCP.** These are one-shot notices between machines
that may not have a connection at all: a sender introduces itself *before* it
opens the stream, and a second sender has to be told "busy" without one. A
datagram fits that shape; a connection would have to be established, kept and
torn down to carry twenty bytes.

**Losing one is not serious**, which is why it can be UDP in the first place.
Every notice degrades to the behaviour that existed before it: a lost `BYE`
means the sender keeps transmitting into a screen nobody is watching until
somebody stops it by hand. If the port is taken, gocast gives up on arbitration
and transmits anyway — a convenience must not become a requirement.

**Every message towards a sender carries a session token**, and this is not
decoration. The control port belongs to the *machine*, not to a run: two
consecutive senders on the same PC listen on the same port. Without a token, the
`BYE` addressed to the run that has just ended is delivered to the one that has
just started, which stops believing it was meant for it. It happens on every
quick restart — changing resolution from the extension is exactly that — and
from outside it looks as if transmission no longer starts at all. The sender
generates a random token, declares it in `HELLO`, and ignores anything that does
not carry it.

**Pairing rides the same channel.** `PAIR` asks the receiver to show a code; the
receiver answers `SHOWING` and paints it on its own screen, or `NOPAIR` if it is
transmitting — while transmitting it cannot show the code without handing it to
whoever is watching, which is precisely who pairing has to keep out. The code
then comes back in a `HELLO`, and the answer is `PAIRED` or `DENY`.

### The receiver dictates the resolution

`kmssink` draws by setting a display mode, and refuses anything that is not one
of the monitor's own modes. An ultrawide scaled to a 1920 ceiling produces
1920x804 — which no TV has among its modes, so the stream dies in negotiation.

So the receiver announces its screen size over mDNS, and the sender fits the
picture inside it with black bars.

### Playback: GStreamer, with ffmpeg as an escape hatch

The default is a single GStreamer process: it demuxes, decodes in hardware and
draws, without raw frames ever leaving the process. About **14% of one core** on
a Pi 3 at 1080p.

`--player ffmpeg` decodes with ffmpeg and pipes raw frames into the same sink.
It costs some six times as much CPU, and exists for one reason: ffmpeg tolerates
damaged streams where GStreamer discards the incomplete access unit. On the same
lossy stream, at the same moment: GStreamer 0 frames, ffmpeg 814.

## Known traps

Everything below was found by measurement, usually after hours of looking in the
wrong place. The comments in the source carry the numbers.

**GStreamer 1.18 cannot drive the V4L2 decoder.** On Bullseye `v4l2h264dec`
accepts the caps and then emits an immediate EOS: zero frames, no error. ffmpeg
decodes the same file on the same hardware at 5.9x real time. Bookworm or newer
is required.

**`pipewiresrc` delivers caps without a framerate.** Declare one anywhere in the
chain and `videoscale` refuses to build its converter — `assertion in_info->fps_n
== out_info->fps_n failed` — the pipeline dies in negotiation and not one byte is
transmitted.

**`pipewiresrc` only delivers a frame when the screen changes.** Share a still
window and the sender transmits 5 kbit/s, which is nothing: whoever is watching
never even sees the first frame. Hence `keepalive-time` on by default.

**`videoscale add-borders` gives up on the portal's source.** The pixel aspect
ratio it receives is unusable ("Can't calculate borders"), and having given up it
distorts the picture to fill the frame. The caps are normalised with `capssetter`
first — and the format has to be forced *before* the label is rewritten, or
`capssetter` declares I420 over BGRA data and corrupts the image.

**The HDMI card often accepts only S/PDIF subframes.** On a Raspberry the
vc4hdmi device refuses plain `S16_LE` and takes `IEC958_SUBFRAME_LE` alone —
and `plughw` does not bridge that gap, because wrapping samples into IEC958 is
the job of the `hdmi:` device:

```
plughw:0,0                 Sample format non available
hdmi:CARD=vc4hdmi,DEV=0    plays
```

A receiver that names one shape of device and gives up when it fails stays mute
in front of a television that works perfectly. gocast builds the candidates from
the card names and tries each one.

**Testing with `videotestsrc` reproduces none of this.** It always has a
framerate, clean caps and content that compresses to nothing. Every fault above
passed the bench and failed on the real source. An honest bench strips the
framerate off the source:

```
videotestsrc ! capssetter caps="video/x-raw,format=BGRA,width=W,height=H" \
    join=false replace=true ! ...
```

## Diagnostics

```console
$ gocast probe          # count the frames the source really delivers
$ gocast crop           # inspect the portal's buffer: opacity, rectangles
$ GOCAST_LOG=1 gocast send    # mirror the log to /tmp/gocast.log
```

To judge picture quality without asking somebody to look at the TV, grab a frame
on the receiver and look at it yourself:

```console
$ ffmpeg -c:v h264_v4l2m2m -i "tcp://0.0.0.0:5000?listen=1" -frames:v 1 -y out.png
```

## Layout

```
cmd/gocast/          entry point, command dispatch
internal/media/      geometry, GStreamer pipelines, element detection, logging
internal/portal/     ScreenCast over D-Bus (Wayland)
internal/control/    control protocol, arbiter, access codes
internal/discovery/  mDNS announcement and search
internal/sender/     send, probe, crop, pair
internal/receiver/   serve, playback, TCP server, pairing screen
gnome-extension/     GNOME Shell extension
```

Dependencies run one way: `media`, `portal` and `control` know nobody;
`discovery` uses only the protocol's port constant; `sender` and `receiver` sit
on top; `cmd` does nothing but dispatch.

## Licence

MIT.
