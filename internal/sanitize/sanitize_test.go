package sanitize

import "testing"

func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain.txt":                "plain.txt",
		"naïve 文件.txt":             "naïve 文件.txt",
		"evil\x1b]0;pwned\x07.txt": "evil?]0;pwned?.txt",
		"line\nbreak":              "line break",
		"c1\u0085next":             "c1?next",
		"bidi\u202efdp.exe":        "bidi?fdp.exe",
		"zero\u200bwidth":          "zero?width",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
