package clientinfo

import (
	"net/url"
	"strings"
	"testing"
)

func TestClientLabelsNormalize(t *testing.T) {
	cases := []struct {
		query string
		want  Info
	}{
		{"app=web&os=windows&browser=chromium", Info{"web", "windows", "chromium"}},
		{"app=desktop&os=macos", Info{"desktop", "macos", Unknown}},
		// The native iOS app (R65, docs/67 D5).
		{"app=ios&os=ios", Info{"ios", "ios", Unknown}},
		{"", Info{Unknown, Unknown, Unknown}},
		{"app=&os=&browser=", Info{Unknown, Unknown, Unknown}},
		// Outside the vocabulary, including case variants and anything a
		// client could use to mint series: always "other".
		{"app=Web&os=Windows%2011&browser=Mozilla%2F5.0", Info{Other, Other, Other}},
		{"app=" + strings.Repeat("x", 4096), Info{Other, Unknown, Unknown}},
		// The first value wins; a repeated parameter cannot smuggle a second.
		{"os=linux&os=bogus", Info{Unknown, "linux", Unknown}},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		if got := FromQuery(q); got != c.want {
			t.Errorf("FromQuery(%q) = %+v, want %+v", c.query, got, c.want)
		}
	}
}
