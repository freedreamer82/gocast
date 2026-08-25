package control

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The access codes already used, per receiver.
//
// Without memory the code would have to be supplied on every transmission, and
// from the GNOME extension it could not be supplied at all: you type it once on
// the command line and from then on the icon in the bar works too.
//
// The file is readable by its owner only. This is not a keyring: the code
// travels in the clear over the network anyway, and protecting it any harder
// would suggest a level of security the rest does not back up.
func pinsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gocast", "pins.json"), nil
}

func loadPins() map[string]string {
	path, err := pinsPath()
	if err != nil {
		return map[string]string{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil || pins == nil {
		return map[string]string{}
	}
	return pins
}

func savePins(pins map[string]string) error {
	path, err := pinsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// RememberPin records the code for a receiver.
func RememberPin(host, pin string) error {
	if host == "" || pin == "" {
		return nil
	}
	pins := loadPins()
	if pins[host] == pin {
		return nil
	}
	pins[host] = pin
	return savePins(pins)
}

// ForgetPin drops a receiver's code once it turns out to be invalid.
//
// Without this, a code changed on the receiver would leave the sender retrying
// the old one forever, with nothing to suggest typing a new one.
func ForgetPin(host string) error {
	pins := loadPins()
	if _, ok := pins[host]; !ok {
		return nil
	}
	delete(pins, host)
	return savePins(pins)
}

func RecallPin(host string) string { return loadPins()[host] }

// ReceiverID returns the receiver's stable identity, generating it on first
// run.
//
// It exists because pairing cannot be tied to the IP address: under DHCP it
// changes, and when it does the sender would have to pair again although
// nothing really changed. With an identity of its own, the receiver stays the
// same wherever it is moved.
func ReceiverID() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "gocast", "id")

	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// The paired codes, on the receiver's side.
//
// Remembering them across restarts avoids pairing again every morning on a box
// that is switched off at night. The "only whoever is in front of the screen"
// requirement applies to the first pairing, which is when the code is shown:
// the same bargain as a remote control that pairs once.
func pairedPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gocast", "paired.json"), nil
}

func loadPaired() map[string]bool {
	path, err := pairedPath()
	if err != nil {
		return map[string]bool{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	var codes []string
	if err := json.Unmarshal(data, &codes); err != nil {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(codes))
	for _, c := range codes {
		set[c] = true
	}
	return set
}

func savePaired(set map[string]bool) error {
	path, err := pairedPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	codes := make([]string, 0, len(set))
	for c := range set {
		codes = append(codes, c)
	}
	sort.Strings(codes) // stable order: the file changes only when the content does
	data, err := json.MarshalIndent(codes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
