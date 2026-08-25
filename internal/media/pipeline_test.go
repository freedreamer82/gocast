package media

import (
	"reflect"
	"testing"
)

func TestSplitPipeline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"plain arguments",
			"-q udpsrc port=5000 ! tsdemux",
			[]string{"-q", "udpsrc", "port=5000", "!", "tsdemux"},
		},
		{
			// Without quotes the on-screen text would be broken into separate
			// arguments and the pipeline would not build.
			"properties containing spaces",
			`textoverlay text="Pairing code: 4821" font-desc="Sans Bold 42"`,
			[]string{"textoverlay", "text=Pairing code: 4821", "font-desc=Sans Bold 42"},
		},
		{
			"repeated spaces and tabs",
			"a  \t b",
			[]string{"a", "b"},
		},
		{
			"empty string",
			"",
			nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SplitPipeline(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitPipeline(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
