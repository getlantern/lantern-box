package main

import "testing"

func TestDatabaseNameFromURL(t *testing.T) {
	for url, want := range map[string]string{
		"https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz":            "GeoLite2-City.mmdb",
		"https://lanterngeo.lantern.io/GeoLite2-Country.mmdb.tar.gz":         "GeoLite2-Country.mmdb",
		"https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz?download=1": "GeoLite2-City.mmdb",
		"https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz#latest":     "GeoLite2-City.mmdb",
	} {
		if got := databaseNameFromURL(url); got != want {
			t.Errorf("databaseNameFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}
