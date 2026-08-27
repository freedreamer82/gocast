/* GoCast — panel indicator for sharing the screen with a receiver on the LAN.
 *
 * The extension is deliberately thin: discovery, the ScreenCast portal and the
 * GStreamer pipelines all live in the gocast binary. What is here is the menu
 * and the lifetime of the child process.
 */

import Clutter from 'gi://Clutter';
import GLib from 'gi://GLib';
import GObject from 'gi://GObject';
import Gio from 'gi://Gio';
import St from 'gi://St';

import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import * as ModalDialog from 'resource:///org/gnome/shell/ui/modalDialog.js';
import * as PanelMenu from 'resource:///org/gnome/shell/ui/panelMenu.js';
import * as PopupMenu from 'resource:///org/gnome/shell/ui/popupMenu.js';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

// Where to look for the binary, in order.
//
// Looking it up ourselves rather than relying on the name alone: gnome-shell is
// started by the display manager, and its PATH is not the one from a login
// shell — on several distributions /usr/local/bin is missing from it, so a
// perfectly installed gocast is reported as "not found" and there is nothing in
// the message to suggest where to look.
//
// The bare name stays last: if the shell does have it on PATH, that is the one
// the user expects to run.
const BINARY_CANDIDATES = [
    '/usr/local/bin/gocast',
    '/usr/bin/gocast',
    GLib.build_filenamev([GLib.get_home_dir(), '.local', 'bin', 'gocast']),
    GLib.build_filenamev([GLib.get_home_dir(), 'bin', 'gocast']),
];

// Resolved once at startup by findBinary(); the bare name is the fallback.
let BINARY = 'gocast';

/* findBinary returns the first candidate that exists and can be executed,
 * falling back to whatever GLib can find on PATH. */
function findBinary() {
    for (const path of BINARY_CANDIDATES) {
        if (GLib.file_test(path, GLib.FileTest.IS_EXECUTABLE))
            return path;
    }
    const onPath = GLib.find_program_in_path('gocast');
    return onPath !== null ? onPath : 'gocast';
}
const SCAN_WAIT = '2s';
const SIGTERM = 15;

// The binary announces the area it is transmitting on its standard output.
const AREA_PREFIX = 'GOCAST-AREA ';

// Offered widths. Anything above what a receiver declares is hidden for that
// receiver: the binary would clamp it anyway, and a menu that offers choices it
// cannot honour is worse than one that omits them.
/* Offered when a receiver is too old to announce its own modes. A modern one
 * lists what its screen actually accepts, which is the only thing that works:
 * the receiver draws by setting a display mode and refuses any other size. */
const FALLBACK_WIDTHS = [1920, 1280, 960, 640];

/* Asks for the code that the receiver is showing on its screen.
 *
 * The code is never sent to whoever asks for it — it appears on the display, so
 * only someone standing in front of it can read it. That is the whole point of
 * the mechanism, and the reason this prompt exists instead of a stored secret.
 */
const PairDialog = GObject.registerClass(
class PairDialog extends ModalDialog.ModalDialog {
    _init(receiverName, onSubmit) {
        super._init({styleClass: 'prompt-dialog'});

        const box = new St.BoxLayout({
            vertical: true,
            style_class: 'prompt-dialog-main-layout',
        });
        box.add_child(new St.Label({
            text: `Pair with ${receiverName}`,
            style_class: 'prompt-dialog-headline headline',
        }));
        box.add_child(new St.Label({
            text: 'Type the code shown on the receiver’s screen.',
            style_class: 'prompt-dialog-description',
        }));

        this._entry = new St.Entry({
            can_focus: true,
            x_expand: true,
            style_class: 'prompt-dialog-password-entry',
        });
        box.add_child(this._entry);
        this.contentLayout.add_child(box);

        this.setButtons([
            {
                label: 'Cancel',
                key: Clutter.KEY_Escape,
                action: () => this.close(),
            },
            {
                label: 'Pair',
                default: true,
                action: () => {
                    const code = this._entry.get_text().trim();
                    this.close();
                    if (code)
                        onSubmit(code);
                },
            },
        ]);
    }

    open() {
        super.open();
        this._entry.grab_key_focus();
    }
});

const Indicator = GObject.registerClass(
class GoCastIndicator extends PanelMenu.Button {
    _init(extensionVersion) {
        super._init(0.0, 'GoCast', false);

        this._extensionVersion = extensionVersion;

        this._icon = new St.Icon({
            icon_name: 'video-display-symbolic',
            style_class: 'system-status-icon',
        });
        this.add_child(this._icon);

        this._proc = null;
        this._activeName = null;
        this._receivers = null;   // null = never scanned, [] = scanned, empty
        this._scanning = false;
        this._force = false;      // take over, cleared after one use
        this._debug = false;      // ask the binary to write a log file

        // Which output's sound to transmit. null = leave it to the binary,
        // which follows whatever the system considers active. Read once and
        // kept: probing the audio server costs a subprocess, and the list does
        // not change while a menu is open.
        this._audioOutputs = null;   // null = never read
        this._audioSource = null;    // null = follow the active output

        // Refresh in the background on open, but keep the previous list on
        // screen: clearing it would show "Searching…" for the whole scan, every
        // single time the menu is opened.
        this.menu.connect('open-state-changed', (_menu, open) => {
            if (open && !this._proc)
                this._scan();
        });

        this._rebuild();
        this._readVersion();
        this._readAudioOutputs();
        this._scan();
    }

    /* Menu ------------------------------------------------------------- */

    _rebuild() {
        this.menu.removeAll();

        if (this._proc) {
            // What is being shared, not just where: with the receiver in
            // another room there is nothing else to tell a whole desktop from
            // a single window.
            const what = {screen: 'whole screen', window: 'one window'}[this._activeKind];
            const label = what
                ? `Stop sharing ${what} — ${this._activeName}`
                : `Stop sharing — ${this._activeName}`;
            const stop = new PopupMenu.PopupMenuItem(label);
            stop.connect('activate', () => this._stop());
            this.menu.addMenuItem(stop);
            return;
        }

        if (this._receivers === null)
            this.menu.addMenuItem(this._inert('Searching…'));
        else if (this._receivers.length === 0)
            this.menu.addMenuItem(this._inert('No screens found'));
        else
            this._receivers.forEach(r => this.menu.addMenuItem(this._receiverItem(r)));

        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        this.menu.addMenuItem(this._settings());
        this.menu.addMenuItem(this._audioMenu());
        this.menu.addMenuItem(this._info());

        const again = new PopupMenu.PopupMenuItem(
            this._scanning ? 'Searching…' : 'Search again');
        again.setSensitive(!this._scanning);
        again.connect('activate', () => this._scan());
        this.menu.addMenuItem(again);
    }

    _inert(text) {
        return new PopupMenu.PopupMenuItem(text, {reactive: false});
    }

    /* A receiver that still needs pairing gets a plain entry that starts the
     * pairing flow. One that is ready gets a submenu of the resolutions it can
     * actually accept. */
    _receiverItem(r) {
        if (r.Pairing && !r.Paired) {
            const item = new PopupMenu.PopupMenuItem(`${r.Name} — pair first`);
            item.connect('activate', () => this._pair(r));
            return item;
        }

        const item = new PopupMenu.PopupSubMenuMenuItem(this._label(r));

        const max = new PopupMenu.PopupMenuItem(
            r.MaxWidth > 0 ? `Highest allowed (${r.MaxWidth}px)` : 'Native resolution');
        max.connect('activate', () => this._start(r, 0));
        item.menu.addMenuItem(max);

        /* The modes the receiver announced, rather than a list of round numbers:
         * asking for a width its screen has no mode for gets the stream refused
         * in negotiation, and from here that looks like "lower resolutions do
         * not work". */
        const modes = (r.Modes || []).filter(m => m.W > 0 && m.W < r.MaxWidth);
        if (modes.length > 0) {
            for (const m of modes) {
                const sub = new PopupMenu.PopupMenuItem(`${m.W}×${m.H}`);
                sub.connect('activate', () => this._start(r, m.W));
                item.menu.addMenuItem(sub);
            }
        } else {
            for (const w of FALLBACK_WIDTHS) {
                if (r.MaxWidth > 0 && w >= r.MaxWidth)
                    continue;
                const sub = new PopupMenu.PopupMenuItem(`${w}px`);
                sub.connect('activate', () => this._start(r, w));
                item.menu.addMenuItem(sub);
            }
        }

        /* A version gap is shown where the choice is made. The two halves are
         * installed separately and drift apart without warning, and the symptoms
         * — an option ignored, a resolution that will not change — point
         * anywhere but here. */
        const note = this._versionNote(r);
        if (note) {
            item.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
            const v = new PopupMenu.PopupMenuItem(note);
            v.setSensitive(false);
            item.menu.addMenuItem(v);
        }
        return item;
    }

    _label(r) {
        const bits = [r.Name];
        if (r.MaxWidth > 0)
            bits.push(`max ${r.MaxWidth}px`);
        return bits.join(' — ');
    }

    _settings() {
        const menu = new PopupMenu.PopupSubMenuMenuItem('Settings');

        const force = new PopupMenu.PopupSwitchMenuItem(
            'Take over if busy (next share only)', this._force);
        force.connect('toggled', (_i, on) => {
            this._force = on;
        });
        // Keep the menu open: whoever flips a switch usually wants to pick a
        // screen right after, and reopening on every toggle is a nuisance.
        force.activate = () => force.toggle();
        menu.menu.addMenuItem(force);

        const debug = new PopupMenu.PopupSwitchMenuItem(
            'Write debug log to /tmp/gocast.log', this._debug);
        debug.connect('toggled', (_i, on) => {
            this._debug = on;
        });
        debug.activate = () => debug.toggle();
        menu.menu.addMenuItem(debug);

        return menu;
    }

    /* _audioMenu chooses which output's sound is transmitted.
     *
     * At the top level and not inside Settings, where it belongs by meaning:
     * gnome-shell does not support a PopupSubMenuMenuItem inside another one.
     * A submenu nested in a submenu builds without complaint and then breaks
     * the menu when it is opened.
     *
     * gocast transmits what one of the PC's outputs is playing, and by default
     * that is whichever the system considers active. A laptop easily has five —
     * a dock, the speakers, three HDMI ports — so a player sending its sound to
     * one of the others produces a silent transmission with nothing anywhere to
     * say why. Until this menu the only cure was a terminal. */
    _audioMenu() {
        const menu = new PopupMenu.PopupSubMenuMenuItem('Audio output');

        // A dot rather than a switch: these are alternatives, only one at a
        // time, and a row of switches would suggest they can be combined.
        //
        // The ornaments are moved by hand instead of rebuilding the menu, and
        // activate is overridden so the item does not close it — the same
        // reason the switches above do it. Whoever changes a setting usually
        // changes another one right after, and a menu that shuts on every click
        // makes that four trips through the panel.
        const items = [];
        const pick = (label, source) => {
            const item = new PopupMenu.PopupMenuItem(label);
            item.setOrnament(this._audioSource === source
                ? PopupMenu.Ornament.DOT : PopupMenu.Ornament.NONE);
            item.activate = () => {
                this._audioSource = source;
                for (const [other, src] of items)
                    other.setOrnament(src === source
                        ? PopupMenu.Ornament.DOT : PopupMenu.Ornament.NONE);
            };
            items.push([item, source]);
            menu.menu.addMenuItem(item);
        };

        pick('Follow the active output', null);

        if (this._audioOutputs === null) {
            menu.menu.addMenuItem(this._inert('Reading…'));
            return menu;
        }
        if (this._audioOutputs.length === 0) {
            // Not an error: without a list the default still works, it simply
            // cannot be chosen from.
            menu.menu.addMenuItem(this._inert('No outputs found'));
            return menu;
        }

        menu.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        for (const o of this._audioOutputs)
            pick(o.default ? `${o.desc} (active)` : o.desc, o.monitor);

        return menu;
    }

    /* _readAudioOutputs asks the binary, which asks GStreamer.
     *
     * Not pactl: on a PipeWire-only system it is not installed, which is the
     * same reason the binary's default is the server-resolved magic name rather
     * than one looked up by hand. A failure here is silent on purpose — the
     * menu falls back to offering the default alone, and a notification about
     * an audio listing nobody asked for would be noise. */
    _readAudioOutputs() {
        let proc;
        try {
            proc = this._launcher(
                Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE
            ).spawnv([BINARY, 'audio', '--json']);
        } catch (e) {
            this._audioOutputs = [];
            return;
        }

        proc.communicate_utf8_async(null, null, (p, res) => {
            let out = '';
            try {
                [, out] = p.communicate_utf8_finish(res);
            } catch (e) {
                this._audioOutputs = [];
                return;
            }
            try {
                const parsed = JSON.parse(out);
                this._audioOutputs = Array.isArray(parsed) ? parsed : [];
            } catch (e) {
                this._audioOutputs = [];
            }
            // The reply outlives the indicator when the extension is disabled
            // while the subprocess is still running, and rebuilding a menu that
            // is already gone throws into the shell's own loop.
            if (this.menu)
                this._rebuild();
        });
    }

    /* _info shows what is installed on each side.
     *
     * The two halves live on different machines and are updated by different
     * commands, so "which versions am I actually running" is the first question
     * when something behaves oddly — and until now it could only be answered
     * from a terminal on both. */
    _info() {
        const menu = new PopupMenu.PopupSubMenuMenuItem('Info');

        const line = text => {
            const i = new PopupMenu.PopupMenuItem(text);
            i.setSensitive(false);
            i.label.clutter_text.line_wrap = true;
            menu.menu.addMenuItem(i);
        };

        line(`Sender: gocast ${this._version || 'unknown'}`);
        line(`Binary: ${BINARY}`);
        line(`Extension: ${this._extensionVersion || 'unknown'}`);

        menu.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());

        if (this._receivers === null)
            line('Receivers: searching…');
        else if (this._receivers.length === 0)
            line('Receivers: none found');
        else {
            for (const r of this._receivers) {
                const screen = r.MaxWidth > 0 ? `${r.MaxWidth}×${r.MaxHeight}` : 'unknown screen';
                line(`${r.Name} — ${r.Host}:${r.Port}`);
                line(`    ${screen} · ${this._versionNote(r)}`);
            }
        }

        return menu;
    }

    /* Versions ---------------------------------------------------------- */

    /* Reads the version of the binary this extension will actually launch.
     *
     * Asked of the binary rather than hard-coded here: the extension and the
     * binary are installed by separate commands, and hard-coding would make the
     * extension confidently report a version it is not running. */
    _readVersion() {
        let proc;
        try {
            proc = this._launcher(Gio.SubprocessFlags.STDOUT_PIPE)
                .spawnv([BINARY, 'version']);
        } catch (e) {
            return;
        }
        proc.communicate_utf8_async(null, null, (p, res) => {
            try {
                const [, out] = p.communicate_utf8_finish(res);
                this._version = (out || '').trim().replace(/^gocast\s+/, '');
            } catch (e) {
                // A binary too old to know the command: left unknown, and the
                // note below says so rather than claiming a match.
            }
        });
    }

    /* _versionNote returns what to show about the version, or '' when there is
     * nothing worth saying. */
    _versionNote(r) {
        if (!r.Version)
            return 'receiver too old to report its version';
        if (this._version && r.Version !== this._version)
            return `receiver v${r.Version} · sender v${this._version} — update one side`;
        return `v${r.Version}`;
    }

    /* Child processes -------------------------------------------------- */

    /* Debug logging is opt-in, so the launcher is where the choice is applied.
     * Writing to a shared file in /tmp on every run would be intrusive. */
    _launcher(flags) {
        const launcher = new Gio.SubprocessLauncher({flags});
        if (this._debug)
            launcher.setenv('GOCAST_LOG', '1', true);
        return launcher;
    }

    _scan() {
        if (this._scanning)
            return;

        let proc;
        try {
            proc = this._launcher(
                Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE
            ).spawnv([BINARY, 'list', '--json', '--wait', SCAN_WAIT]);
        } catch (e) {
            this._receivers = [];
            this._fail(
                `gocast not found — looked in ${BINARY_CANDIDATES.join(', ')} ` +
                `and on PATH. Install it with: sudo install -m755 gocast /usr/local/bin/`,
                e);
            return;
        }

        this._scanning = true;
        this._rebuild();

        proc.communicate_utf8_async(null, null, (p, res) => {
            this._scanning = false;
            try {
                const [, stdout, stderr] = p.communicate_utf8_finish(res);
                if (!p.get_successful()) {
                    this._receivers = [];
                    this._fail('Search failed', new Error(this._errorLine(stderr)));
                    return;
                }
                this._receivers = JSON.parse(stdout);
                this._rebuild();
            } catch (e) {
                this._receivers = [];
                this._fail('Unreadable reply', e);
            }
        });
    }

    /* Pairing runs the binary, which asks the receiver to show a code, then
     * reads the code from its standard input — which is what the dialog feeds
     * it. */
    _pair(r) {
        let proc;
        try {
            proc = this._launcher(
                Gio.SubprocessFlags.STDIN_PIPE | Gio.SubprocessFlags.STDERR_PIPE
            ).spawnv([BINARY, 'pair', '--host', r.Host, '--port', String(r.Port)]);
        } catch (e) {
            this._fail('Could not start pairing', e);
            return;
        }

        new PairDialog(r.Name, code => {
            proc.get_stdin_pipe().write_all(`${code}\n`, null);
        }).open();

        proc.communicate_utf8_async(null, null, (p, res) => {
            let stderr = '';
            try {
                [, , stderr] = p.communicate_utf8_finish(res);
            } catch (e) {
                // Only the exit status matters; stderr is a bonus.
            }
            if (p.get_successful()) {
                Main.notify('GoCast', `Paired with ${r.Name}.`);
                this._scan(); // the entry becomes a resolution submenu
            } else {
                Main.notifyError('GoCast',
                    this._errorLine(stderr) || `Pairing with ${r.Name} failed`);
            }
        });
    }

    _start(r, width) {
        const argv = [BINARY, 'send', '--host', r.Host, '--port', String(r.Port)];
        // The binary clamps this to what the receiver declared: asking for less
        // is allowed, asking for more is not.
        if (width > 0)
            argv.push('--width', String(width));
        if (this._force)
            argv.push('--force');
        // Only when chosen: with no flag the binary follows the active output,
        // which is what most people want and what it does on its own.
        if (this._audioSource)
            argv.push('--audio-source', this._audioSource);

        try {
            this._proc = this._launcher(
                Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE
            ).spawnv(argv);
        } catch (e) {
            this._fail('Could not start sharing', e);
            return;
        }

        // One-shot: leaving it armed would silently push a colleague off the
        // screen on the next share.
        this._force = false;

        this._activeName = r.Name;
        this._icon.add_style_class_name('gocast-active');
        this._rebuild();

        // Read line by line rather than waiting for the process to finish: the
        // label has to say what is being shared while it is happening.
        this._readLines(this._proc.get_stdout_pipe(), line => {
            if (!line.startsWith(AREA_PREFIX))
                return;
            // "<width>x<height>+<x>+<y> <screen|window>" — della geometria non
            // ce ne facciamo niente: sono coordinate interne al buffer del
            // portale, non posizioni sullo schermo. Serve solo il tipo.
            const [area, kind] = line.slice(AREA_PREFIX.length).trim().split(' ');
            this._activeKind = kind;
            if (kind === 'window')
                this._watchSharedWindow(area);
            this._rebuild();
        });

        let stderr = '';
        this._readLines(this._proc.get_stderr_pipe(), line => {
            stderr += `${line}\n`;
        });

        this._proc.wait_async(null, (p, res) => {
            try {
                p.wait_finish(res);
            } catch (e) {
                // Only the exit status matters.
            }

            const name = this._activeName;
            const clean = p.get_successful() || p.get_if_signaled();

            this._unwatchWindow();
            this._proc = null;
            this._activeName = null;
            this._activeKind = null;
            this._icon.remove_style_class_name('gocast-active');
            this._rebuild();

            if (!clean) {
                // The journal gets the whole of stderr: the last line is always
                // gocast's own summary and hides the real GStreamer error a few
                // lines above it.
                console.error(`GoCast: sharing to ${name} failed\n${stderr}`);

                if (/busy|occupato/i.test(stderr ?? '')) {
                    Main.notifyError('GoCast',
                        `${name} is already in use. Enable “Take over if busy” and retry.`);
                } else {
                    Main.notifyError('GoCast',
                        this._errorLine(stderr) || `Sharing to ${name} stopped`);
                }
            }
        });
    }


    /* Feeds each line to onLine as it arrives, then stops at end of stream. */
    _readLines(stream, onLine) {
        const input = new Gio.DataInputStream({base_stream: stream});
        const next = () => {
            input.read_line_async(GLib.PRIORITY_DEFAULT, null, (source, res) => {
                let line = null;
                try {
                    [line] = source.read_line_finish_utf8(res);
                } catch (e) {
                    return; // stream closed under us: nothing left to read
                }
                if (line === null)
                    return; // end of stream
                onLine(line);
                next();
            });
        };
        next();
    }

    /* Stops the share when the shared window is closed.
     *
     * The portal does not end the session on its own: it keeps handing over a
     * buffer that no longer shows anything, and the binary has no way to tell
     * an empty capture from a still screen. Nothing here draws — the window is
     * looked up only to learn when it goes away.
     *
     * Matching is by size, which is all the portal reveals. Two windows of the
     * same size make it ambiguous, and watching the wrong one would stop a
     * healthy share: in that case nothing is watched.
     */
    _watchSharedWindow(area) {
        this._unwatchWindow();

        const m = /^(\d+)x(\d+)/.exec(area ?? '');
        if (!m)
            return;
        const [w, h] = m.slice(1).map(Number);

        const tolerance = 8;
        const found = global.get_window_actors()
            .map(a => a.meta_window)
            .filter(win => {
                if (!win || win.minimized)
                    return false;
                const r = win.get_frame_rect();
                return Math.abs(r.width - w) <= tolerance &&
                       Math.abs(r.height - h) <= tolerance;
            });
        if (found.length !== 1)
            return;

        this._sharedWindow = found[0];
        this._sharedWindowId = this._sharedWindow.connect('unmanaged', () => {
            this._unwatchWindow();
            if (this._proc) {
                Main.notify('GoCast', 'The shared window was closed: sharing stopped.');
                this._stop();
            }
        });
    }

    _unwatchWindow() {
        if (!this._sharedWindow)
            return;
        this._sharedWindow.disconnect(this._sharedWindowId);
        this._sharedWindow = null;
        this._sharedWindowId = 0;
    }

    _stop() {
        // SIGTERM rather than force_exit: gocast catches it and closes the
        // portal session, otherwise GNOME keeps showing the screen-sharing
        // indicator for a share that no longer exists.
        this._proc?.send_signal(SIGTERM);
    }

    /* Diagnostics ------------------------------------------------------ */

    _lastLine(text) {
        if (!text)
            return '';
        const lines = text.trim().split('\n').filter(l => l.trim() !== '');
        return lines.length ? lines[lines.length - 1] : '';
    }

    /* The line that explains the failure, not the last one: gocast always exits
     * with a generic summary that would mask the diagnosis. */
    _errorLine(text) {
        if (!text)
            return '';
        const lines = text.trim().split('\n').filter(l => l.trim() !== '');
        const meaningful = lines.filter(l =>
            /error|errore|fail|fallit|portal|portale/i.test(l) &&
            !/exit status/i.test(l));
        return meaningful.length ? meaningful[0] : this._lastLine(text);
    }

    _fail(summary, err) {
        console.error(`GoCast: ${summary}: ${err?.message ?? err}`);
        this._rebuild();
        Main.notifyError('GoCast', `${summary}: ${err?.message ?? err}`);
    }

    destroy() {
        this._unwatchWindow();
        this._proc?.send_signal(SIGTERM);
        super.destroy();
    }
});

export default class GoCastExtension extends Extension {
    enable() {
        // Resolved here rather than at import time: the binary may well be
        // installed while the extension is loaded, and enabling it again is a
        // cheaper fix to suggest than a logout.
        BINARY = findBinary();
        console.log(`gocast: using ${BINARY}`);

        this._indicator = new Indicator(
            this.metadata['version-name'] || String(this.metadata.version || ''));
        Main.panel.addToStatusArea(this.uuid, this._indicator);
    }

    disable() {
        this._indicator?.destroy();
        this._indicator = null;
    }
}
