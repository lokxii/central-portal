package progress

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with stderr pointed at a pipe. The pipe is not a
// terminal, so bars created inside fn take the quiet path and can be observed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}
	return string(out)
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1 << 20, "1.0 MB"},
		{1 << 30, "1.0 GB"},
	}
	for _, tc := range cases {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetTotalNegative(t *testing.T) {
	b := New("file.bin", -1)
	if b.total != 0 {
		t.Fatalf("New with a negative total set total = %d, want 0", b.total)
	}
	b.SetTotal(2048)
	if b.total != 2048 {
		t.Fatalf("SetTotal(2048) set total = %d", b.total)
	}
	b.SetTotal(-1)
	if b.total != 0 {
		t.Fatalf("SetTotal(-1) set total = %d, want 0", b.total)
	}
}

func TestNonTTYWriteAndDone(t *testing.T) {
	var bar *Bar
	out := captureStderr(t, func() {
		bar = New("file.bin", 2048)
		if bar.tty {
			t.Fatal("bar was created as a terminal; the test needs the quiet path")
		}
		for _, chunk := range []string{"abcde", "fghij"} {
			n, err := bar.Write([]byte(chunk))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != len(chunk) {
				t.Fatalf("Write returned %d, want %d", n, len(chunk))
			}
		}
		bar.Done()
	})

	if !strings.Contains(out, "file.bin: 10 B in ") {
		t.Fatalf("summary = %q, want the label and transferred size", out)
	}
	if !strings.Contains(out, "/s)") {
		t.Fatalf("summary = %q, want a rate", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("output = %q, want exactly one line", out)
	}
}

func TestNonTTYWriteUnknownTotal(t *testing.T) {
	out := captureStderr(t, func() {
		b := New("stream", 0)
		b.Write([]byte("hello"))
		b.Done()
	})
	if !strings.Contains(out, "stream: 5 B in ") {
		t.Fatalf("summary = %q, want the amount transferred", out)
	}
}

func TestDoneIdempotent(t *testing.T) {
	out := captureStderr(t, func() {
		b := New("file.bin", 4)
		b.Write([]byte("data"))
		b.Done()
		b.Done()
		b.Done()
	})
	if got := strings.Count(out, "file.bin:"); got != 1 {
		t.Fatalf("summary printed %d times, want 1: %q", got, out)
	}
}

func TestCleanLabel(t *testing.T) {
	cases := map[string]string{
		"plain.txt":                "plain.txt",
		"naïve 文件.txt":             "naïve 文件.txt",
		"evil\x1b]0;pwned\x07.txt": "evil?]0;pwned?.txt",
		"del\x7fchar":              "del?char",
		"line\nbreak":              "line break",
		"c1\u0085next":             "c1?next",
		"bidi\u202efdp.exe":        "bidi?fdp.exe",
		"zero\u200bwidth":          "zero?width",
	}
	for in, want := range cases {
		if got := cleanLabel(in); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
