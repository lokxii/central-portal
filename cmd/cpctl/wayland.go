// Wayland clipboard access for the CLI. Only wl-clipboard is supported; on a
// machine without Wayland every operation fails before any network traffic.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"central-portal/internal/model"
	"central-portal/internal/sanitize"
)

const (
	// clipboardCommandTimeout bounds every wl-paste/wl-copy invocation. A
	// compositor or tool that hangs must not stall the command forever.
	clipboardCommandTimeout = 30 * time.Second
	// maxListTypesBytes is generous for a MIME list but stops a runaway
	// producer from exhausting memory.
	maxListTypesBytes = 1 << 20
	maxPasteBytes     = 16 << 20
)

// clipboardImageTypes lists the image formats the portal accepts, in order of
// preference. SVG is deliberately absent: registry content must not be able to
// execute in the portal origin.
var clipboardImageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// requireWayland checks that a Wayland session exists and that the named tools
// are installed.
func requireWayland(tools ...string) error {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no Wayland session: WAYLAND_DISPLAY is not set")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found: install wl-clipboard", tool)
		}
	}
	return nil
}

// readWaylandClipboard returns the clipboard bytes and their media type. An
// image is preferred over text, matching the portal's one-entry registry.
func readWaylandClipboard(ctx context.Context) ([]byte, string, error) {
	if err := requireWayland("wl-paste"); err != nil {
		return nil, "", err
	}

	out, err := runClipboardCommand(ctx, clipboardCommandTimeout, maxListTypesBytes, nil,
		"wl-paste", "--list-types")
	if err != nil {
		return nil, "", err
	}

	var types []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			types = append(types, line)
		}
	}
	mime, isText := chooseClipboardType(types)
	if mime == "" {
		return nil, "", errors.New("wayland clipboard has no text or image content")
	}

	args := []string{"--type", mime}
	if isText {
		args = append([]string{"--no-newline"}, args...)
	}
	data, err := runClipboardCommand(ctx, clipboardCommandTimeout, maxPasteBytes, nil, "wl-paste", args...)
	if err != nil {
		return nil, "", err
	}
	if isText {
		return data, model.ContentTypeText, nil
	}
	return data, mime, nil
}

// chooseClipboardType picks the best offered type: the first supported image,
// otherwise the first text type.
func chooseClipboardType(types []string) (string, bool) {
	for _, want := range clipboardImageTypes {
		for _, offered := range types {
			if strings.EqualFold(offered, want) {
				return want, false
			}
		}
	}
	for _, offered := range types {
		if strings.HasPrefix(strings.ToLower(offered), "text/") {
			return offered, true
		}
	}
	return "", false
}

// writeWaylandClipboard replaces the Wayland clipboard with data, labelled with
// the given media type.
func writeWaylandClipboard(ctx context.Context, data []byte, contentType string) error {
	if err := requireWayland("wl-copy"); err != nil {
		return err
	}
	mime := contentType
	if mime == "" || strings.HasPrefix(strings.ToLower(mime), "text/") {
		mime = "text/plain;charset=utf-8"
	}
	_, err := runClipboardCommand(ctx, clipboardCommandTimeout, maxListTypesBytes,
		bytes.NewReader(data), "wl-copy", "--type", mime)
	return err
}

// errOutputLimit reports that a command produced more output than allowed.
var errOutputLimit = errors.New("output exceeds limit")

// boundedBuffer collects command output up to limit bytes. Once the limit is
// crossed it fails writes, which makes the child process fail rather than
// letting it allocate without bound.
type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - int64(b.buf.Len())
	if int64(len(p)) <= remaining {
		return b.buf.Write(p)
	}
	if remaining > 0 {
		b.buf.Write(p[:remaining])
	}
	b.overflow = true
	return int(remaining), errOutputLimit
}

// runClipboardCommand runs one wl-clipboard tool with a timeout, a bounded
// stdout and captured stderr. A nil stdin leaves standard input untouched.
func runClipboardCommand(ctx context.Context, timeout time.Duration, limit int64, stdin io.Reader, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout boundedBuffer
	stdout.limit = limit
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = stdin

	err := cmd.Run()
	if stdout.overflow {
		return nil, fmt.Errorf("%s output is larger than %d MiB", name, limit>>20)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", name, timeout)
		}
		return nil, commandError(name, err, stderr.String())
	}
	return stdout.buf.Bytes(), nil
}

// commandError keeps the tool's own diagnostic visible. The text is sanitized
// because it is printed to a terminal.
func commandError(name string, err error, stderr string) error {
	if msg := sanitize.Clean(strings.TrimSpace(stderr)); msg != "" {
		return fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return fmt.Errorf("%s: %w", name, err)
}
