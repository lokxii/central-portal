package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChooseClipboardType(t *testing.T) {
	cases := []struct {
		name     string
		types    []string
		want     string
		wantText bool
	}{
		{
			name:     "prefers png over text",
			types:    []string{"text/plain", "image/png"},
			want:     "image/png",
			wantText: false,
		},
		{
			name:     "preference order among images",
			types:    []string{"image/gif", "image/webp", "image/jpeg"},
			want:     "image/jpeg",
			wantText: false,
		},
		{
			name:     "matches image type case-insensitively",
			types:    []string{"IMAGE/PNG"},
			want:     "image/png",
			wantText: false,
		},
		{
			name:     "falls back to first text type",
			types:    []string{"application/x-foo", "text/html", "text/plain"},
			want:     "text/html",
			wantText: true,
		},
		{
			name:     "rejects unsupported images",
			types:    []string{"image/svg+xml"},
			want:     "",
			wantText: false,
		},
		{
			name:     "empty list",
			types:    nil,
			want:     "",
			wantText: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, isText := chooseClipboardType(tc.types)
			if got != tc.want || isText != tc.wantText {
				t.Errorf("chooseClipboardType(%v) = (%q, %v), want (%q, %v)",
					tc.types, got, isText, tc.want, tc.wantText)
			}
		})
	}
}

func TestBoundedBufferRejectsOverflow(t *testing.T) {
	b := &boundedBuffer{limit: 5}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("first write = (%d, %v), want (3, nil)", n, err)
	}
	if n, err := b.Write([]byte("defgh")); n != 2 || err == nil {
		t.Fatalf("overflowing write = (%d, %v), want (2, error)", n, err)
	}
	if !b.overflow {
		t.Error("overflow flag not set")
	}
	if got := b.buf.String(); got != "abcde" {
		t.Errorf("buffer = %q, want %q", got, "abcde")
	}
}

func TestRunClipboardCommandCapturesBoundedOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out, err := runClipboardCommand(context.Background(), time.Second, 1024, nil,
		"sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("runClipboardCommand: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

func TestRunClipboardCommandRejectsOversizedOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	_, err := runClipboardCommand(context.Background(), time.Second, 5, nil,
		"sh", "-c", "printf 0123456789")
	if err == nil {
		t.Fatal("runClipboardCommand accepted oversized output")
	}
	if !strings.Contains(err.Error(), "output is larger than") {
		t.Errorf("error = %v, want an output-limit error", err)
	}
}

// TestRunClipboardCommandDetachedDoesNotWaitForForkedChild guards against the
// wl-copy hang: the tool exits, but a forked child keeps its inherited stdio
// open while it serves the clipboard. Wait must not follow that child.
func TestRunClipboardCommandDetachedDoesNotWaitForForkedChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	start := time.Now()
	err := runClipboardCommandDetached(context.Background(), 5*time.Second, nil,
		"sh", "-c", "sleep 2 &")
	if err != nil {
		t.Fatalf("runClipboardCommandDetached: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v for a forked child, want an immediate return", elapsed)
	}
}

func TestRunClipboardCommandDetachedReportsStderr(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	err := runClipboardCommandDetached(context.Background(), time.Second, nil,
		"sh", "-c", "echo boom >&2; exit 1")
	if err == nil {
		t.Fatal("runClipboardCommandDetached accepted a failing command")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want the command's stderr", err)
	}
}

func TestWriteWaylandClipboardReturnsWhenCopyForks(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 2 &\ncat >/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "wl-copy"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake wl-copy: %v", err)
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	if err := writeWaylandClipboard(context.Background(), []byte("hello"), "text/plain"); err != nil {
		t.Fatalf("writeWaylandClipboard: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("writeWaylandClipboard waited %v for the forked copy, want an immediate return", elapsed)
	}
}

func TestRunClipboardCommandTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	start := time.Now()
	_, err := runClipboardCommand(context.Background(), 100*time.Millisecond, 1024, nil,
		"sh", "-c", "sleep 5")
	if err == nil {
		t.Fatal("runClipboardCommand did not time out")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v, want much less", elapsed)
	}
}
