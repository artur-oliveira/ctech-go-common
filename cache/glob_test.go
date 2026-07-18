package cache

import "testing"

func TestGlobEscaper(t *testing.T) {
	cases := map[string]string{
		"org:1:": "org:1:",
		"org:*:": `org:\*:`,
		"a?b":    `a\?b`,
		"a[b]c":  `a\[b\]c`,
		`a\b`:    `a\\b`,
	}
	for in, want := range cases {
		if got := globEscaper.Replace(in); got != want {
			t.Errorf("globEscaper.Replace(%q) = %q, want %q", in, got, want)
		}
	}
}
