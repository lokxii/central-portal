// Wayland clipboard access for the CLI. Only wl-clipboard is supported; on a
// machine without Wayland every operation fails before any network traffic.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"central-portal/internal/model"
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

	var stderr bytes.Buffer
	list := exec.CommandContext(ctx, "wl-paste", "--list-types")
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		return nil, "", commandError("wl-paste --list-types", err, stderr.String())
	}

	var types []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			types = append(types, line)
		}
	}
	mime, isText := chooseClipboardType(types)
	if mime == "" {
		return nil, "", errors.New("Wayland clipboard has no text or image content")
	}

	args := []string{"--type", mime}
	if isText {
		args = append([]string{"--no-newline"}, args...)
	}
	stderr.Reset()
	paste := exec.CommandContext(ctx, "wl-paste", args...)
	paste.Stderr = &stderr
	data, err := paste.Output()
	if err != nil {
		return nil, "", commandError("wl-paste", err, stderr.String())
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
	var stderr bytes.Buffer
	copyCmd := exec.CommandContext(ctx, "wl-copy", "--type", mime)
	copyCmd.Stdin = bytes.NewReader(data)
	copyCmd.Stderr = &stderr
	if err := copyCmd.Run(); err != nil {
		return commandError("wl-copy", err, stderr.String())
	}
	return nil
}

// commandError keeps the tool's own diagnostic visible.
func commandError(name string, err error, stderr string) error {
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return fmt.Errorf("%s: %w", name, err)
}
