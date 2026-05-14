package mitmdf

import (
	"testing"

	"github.com/getlantern/lantern-box/option"
)

func TestBuildFronts_Validation(t *testing.T) {
	cases := []struct {
		name string
		in   []option.MITMDFFrontEntry
		err  bool
	}{
		{
			name: "empty list",
			in:   nil,
			err:  true,
		},
		{
			name: "missing fronted_sni",
			in:   []option.MITMDFFrontEntry{{Names: []string{"a.com"}}},
			err:  true,
		},
		{
			name: "empty names",
			in:   []option.MITMDFFrontEntry{{FrontedSNI: "front.test"}},
			err:  true,
		},
		{
			name: "empty name entry",
			in:   []option.MITMDFFrontEntry{{Names: []string{""}, FrontedSNI: "front.test"}},
			err:  true,
		},
		{
			name: "valid single",
			in: []option.MITMDFFrontEntry{
				{Names: []string{"vercel.app", "vercel.com"}, FrontedSNI: "nextjs.org"},
			},
			err: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildFronts(tc.in)
			if tc.err && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBuildFronts_Normalizes(t *testing.T) {
	f, err := buildFronts([]option.MITMDFFrontEntry{
		{
			Names:        []string{" Vercel.app ", "VERCEL.com", "vercel.app"}, // duplicate + mixed case
			FrontedSNI:   "Nextjs.org",
			VerifySAN:    []string{"vercel.app", "Vercel.com"},
			RedirectAddr: "nextjs.org:443",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f[0].frontedSNI; got != "nextjs.org" {
		t.Errorf("frontedSNI = %q, want lowercased", got)
	}
	if got := f[0].names; len(got) != 2 {
		t.Errorf("names = %v, want 2 unique lowercased entries", got)
	}
	if got := f[0].verifySAN; len(got) != 2 || got[0] != "vercel.app" {
		t.Errorf("verifySAN = %v, want lowercased pair", got)
	}
}

func TestFrontsLookup_MatchSemantics(t *testing.T) {
	f, err := buildFronts([]option.MITMDFFrontEntry{
		{Names: []string{"vercel.app"}, FrontedSNI: "nextjs.org"},
		{Names: []string{"googlevideo.com"}, FrontedSNI: "www.google.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		sni  string
		want string // frontedSNI, or "" for no match
	}{
		{"vercel.app", "nextjs.org"},
		{"my-app.vercel.app", "nextjs.org"},
		{"deep.sub.vercel.app", "nextjs.org"},
		{"r1---sn-aaa.googlevideo.com", "www.google.com"},
		{"notvercel.app", ""},               // suffix lex-match without `.` boundary
		{"vercel.app.evil.com", ""},         // strict suffix only
		{"vercelapp", ""},                   // no dot
		{"other.com", ""},                   // unrelated
		{"", ""},                            // empty
	}
	for _, c := range cases {
		t.Run(c.sni, func(t *testing.T) {
			got := f.lookup(c.sni)
			if c.want == "" {
				if got != nil {
					t.Errorf("lookup(%q) = %v, want nil", c.sni, got.frontedSNI)
				}
				return
			}
			if got == nil {
				t.Fatalf("lookup(%q) = nil, want match on %q", c.sni, c.want)
			}
			if got.frontedSNI != c.want {
				t.Errorf("lookup(%q).frontedSNI = %q, want %q", c.sni, got.frontedSNI, c.want)
			}
		})
	}
}

func TestDenyMatch(t *testing.T) {
	deny := normalizeDeny([]string{"chase.com", " Bank.example "})
	for _, sni := range []string{"chase.com", "secure.chase.com", "bank.example", "x.bank.example"} {
		if !denyMatch(deny, sni) {
			t.Errorf("denyMatch(%q) = false, want true", sni)
		}
	}
	for _, sni := range []string{"jpmchase.com", "bank.example.com", "vercel.app", ""} {
		if denyMatch(deny, sni) {
			t.Errorf("denyMatch(%q) = true, want false", sni)
		}
	}
}

func TestNormalizeDeny_DedupAndDrop(t *testing.T) {
	got := normalizeDeny([]string{"A.com", " a.com ", "", "B.com", "a.com."})
	want := []string{"a.com", "b.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPresetFromString(t *testing.T) {
	for _, in := range []string{"", "chrome", "firefox", "safari", "random"} {
		if _, err := presetFromString(in); err != nil {
			t.Errorf("presetFromString(%q): %v", in, err)
		}
	}
	for _, in := range []string{"opera", "CHROME", "Chrome", "junk"} {
		if _, err := presetFromString(in); err == nil {
			t.Errorf("presetFromString(%q): want error, got nil", in)
		}
	}
}
