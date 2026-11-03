package unbounded

import "testing"

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want byteRate
	}{
		{"", 0},
		{"0", 0},
		{"5mbit", 625_000},
		{"5Mbit/s", 625_000},
		{"20Mbps", 2_500_000},
		{"8kb", 1000},
		{"500KB/s", 500_000},
		{"500KB", 500_000},
		{"1.5MiB/s", 1_572_864},
		{"2MB", 2_000_000},
		{"1gbit", 125_000_000},
		{"100B/s", 100},
	}
	for _, c := range cases {
		got, err := parseRate(c.in)
		if err != nil {
			t.Errorf("parseRate(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseRate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"fast", "5", "5 mb/h", "-1mbit", "1bit"} {
		if _, err := parseRate(bad); err == nil {
			t.Errorf("parseRate(%q) accepted", bad)
		}
	}
}
