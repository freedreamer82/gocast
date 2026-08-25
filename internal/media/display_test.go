package media

import "testing"

func TestParseMode(t *testing.T) {
	cases := []struct {
		line string
		want Rect
	}{
		{"1920x1080", Rect{W: 1920, H: 1080}},
		{"1920x1080i60", Rect{W: 1920, H: 1080}}, // with the refresh rate appended
		{"1280x720", Rect{W: 1280, H: 720}},
		{"", Rect{}},
		{"garbage", Rect{}},
		{"1920x", Rect{}},
	}
	for _, c := range cases {
		if got := parseMode(c.line); got != c.want {
			t.Errorf("parseMode(%q) = %+v, want %+v", c.line, got, c.want)
		}
	}
}
