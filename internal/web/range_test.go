package web

import (
	"slices"
	"testing"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		h          string
		start, end int64
		ok         bool
	}{
		{"bytes=0-1", 0, 1, true},
		{"bytes=100-", 100, -1, true},
		{"bytes=5-5", 5, 5, true},
		{"bytes= 7-9", 7, 9, true},
		// Not taken: the whole file goes.
		{"", 0, 0, false},
		{"bytes=-500", 0, 0, false},
		{"bytes=0-1,4-5", 0, 0, false},
		{"bytes=9-3", 0, 0, false},
		{"bytes=a-b", 0, 0, false},
		{"bytes=-1-2", 0, 0, false},
		{"items=0-1", 0, 0, false},
		{"bytes=0", 0, 0, false},
	}
	for _, c := range cases {
		start, end, ok := parseRange(c.h)
		if start != c.start || end != c.end || ok != c.ok {
			t.Errorf("parseRange(%q) = %d, %d, %v; want %d, %d, %v", c.h, start, end, ok, c.start, c.end, c.ok)
		}
	}
}

func TestConnectSourcesListEachOnce(t *testing.T) {
	got := connectSources("wss://chat.example.com", []string{"wss://chat.example.com", "wss://voice.example.com/", "wss://voice.example.com"})
	want := []string{"'self'", "wss://chat.example.com", "wss://voice.example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("connectSources = %v, want %v", got, want)
	}
}
