package control

import (
	"os"
	"testing"
)

// isolateConfig points the code store at a temporary directory: tests must not
// touch the user's real configuration.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestPinsRoundTrip(t *testing.T) {
	isolateConfig(t)

	if got := RecallPin("192.168.1.60"); got != "" {
		t.Fatalf("want no stored code, got %q", got)
	}

	if err := RememberPin("192.168.1.60", "4821"); err != nil {
		t.Fatalf("storing failed: %v", err)
	}
	if got := RecallPin("192.168.1.60"); got != "4821" {
		t.Errorf("want 4821, got %q", got)
	}

	t.Run("codes are per receiver", func(t *testing.T) {
		if got := RecallPin("192.168.1.61"); got != "" {
			t.Errorf("one receiver's code must not apply to another: %q", got)
		}
	})

	t.Run("a refused code is forgotten", func(t *testing.T) {
		// Without this, once the code on the receiver changes the sender would
		// retry the old one forever.
		if err := ForgetPin("192.168.1.60"); err != nil {
			t.Fatalf("removal failed: %v", err)
		}
		if got := RecallPin("192.168.1.60"); got != "" {
			t.Errorf("the code should have been forgotten, got %q", got)
		}
	})
}

func TestPinsFileIsPrivate(t *testing.T) {
	isolateConfig(t)

	if err := RememberPin("host", "1234"); err != nil {
		t.Fatalf("storing failed: %v", err)
	}
	path, err := pinsPath()
	if err != nil {
		t.Fatalf("path not determined: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("want 0600, got %o", perm)
	}
}

func TestPinsIgnoresCorruptFile(t *testing.T) {
	isolateConfig(t)

	// An unreadable file must not stop anybody from transmitting: we start again
	// from no stored code, and at worst it gets typed in once more.
	if err := RememberPin("host", "1234"); err != nil {
		t.Fatalf("storing failed: %v", err)
	}
	path, _ := pinsPath()
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	if got := RecallPin("host"); got != "" {
		t.Errorf("want no code from a corrupt file, got %q", got)
	}
	if err := RememberPin("host", "5678"); err != nil {
		t.Errorf("a corrupt file must be overwritable: %v", err)
	}
	if got := RecallPin("host"); got != "5678" {
		t.Errorf("want 5678, got %q", got)
	}
}

func TestRememberIgnoresEmptyValues(t *testing.T) {
	isolateConfig(t)

	if err := RememberPin("", "1234"); err != nil {
		t.Errorf("empty host: %v", err)
	}
	if err := RememberPin("host", ""); err != nil {
		t.Errorf("empty code: %v", err)
	}
	// An open receiver must leave no trace in the configuration.
	if got := RecallPin("host"); got != "" {
		t.Errorf("stored an empty code: %q", got)
	}
}
