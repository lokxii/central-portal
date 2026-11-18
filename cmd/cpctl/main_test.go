package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"central-portal/internal/certs"
	"central-portal/internal/model"
)

// testCAPEM creates a CA in a temporary directory and returns its PEM and
// fingerprint.
func testCAPEM(t *testing.T) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := certs.Ensure(dir, []string{"localhost"}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	pemBytes, err := os.ReadFile(certs.CAPath(dir))
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	fingerprint, err := certs.Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return pemBytes, fingerprint
}

func serveCertBody(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runCACommand(t *testing.T, url, out string, extra ...string) error {
	t.Helper()
	args := append([]string{"-ca-url", url, "-out", out}, extra...)
	var stdout bytes.Buffer
	return caCommand(context.Background(), args, &stdout)
}

func TestCACommandRejectsCertificateBundle(t *testing.T) {
	first, _ := testCAPEM(t)
	second, _ := testCAPEM(t)
	body := append(append([]byte{}, first...), second...)
	srv := serveCertBody(t, body)

	out := filepath.Join(t.TempDir(), "ca.crt")
	err := runCACommand(t, srv.URL, out)
	if err == nil {
		t.Fatal("caCommand accepted a bundle with two certificates")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error = %v, want a message about exactly one certificate", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("caCommand wrote the CA file despite rejecting the bundle")
	}
}

func TestCACommandFingerprintMismatchWritesNothing(t *testing.T) {
	body, _ := testCAPEM(t)
	_, other := testCAPEM(t)
	srv := serveCertBody(t, body)

	out := filepath.Join(t.TempDir(), "ca.crt")
	err := runCACommand(t, srv.URL, out, "-fingerprint", other)
	if err == nil {
		t.Fatal("caCommand accepted a mismatched fingerprint")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error = %v, want a fingerprint mismatch", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("caCommand wrote the CA file despite a fingerprint mismatch")
	}
}

func TestCACommandAcceptsFingerprintWithoutColons(t *testing.T) {
	body, fingerprint := testCAPEM(t)
	srv := serveCertBody(t, body)

	out := filepath.Join(t.TempDir(), "ca.crt")
	loose := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	if err := runCACommand(t, srv.URL, out, "-fingerprint", loose); err != nil {
		t.Fatalf("caCommand with matching fingerprint: %v", err)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read saved CA: %v", err)
	}
	parsed, err := parseCertificates(saved)
	if err != nil {
		t.Fatalf("parse saved CA: %v", err)
	}
	if len(parsed) != 1 {
		t.Errorf("saved file contains %d certificates, want 1", len(parsed))
	}
	if got := certs.FormatFingerprint(parsed[0]); got != fingerprint {
		t.Errorf("saved fingerprint = %q, want %q", got, fingerprint)
	}
}

func TestFetchCARejectsCrossHostRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nope"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	if _, err := fetchCA(context.Background(), origin.URL); err == nil {
		t.Fatal("fetchCA followed a cross-host redirect")
	} else if !strings.Contains(err.Error(), "refuse redirect") {
		t.Errorf("error = %v, want a refused redirect", err)
	}
}

func TestDescribeSanitizesServerText(t *testing.T) {
	entry := &model.Entry{
		Name:        "evil\x1b]0;pwned\x07.txt",
		ContentType: "image/png\x1b[2J",
		Size:        42,
	}
	got := describe(entry, true)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("describe left control characters in %q", got)
	}
}
