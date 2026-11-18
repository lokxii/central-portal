package certs

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestEnsureCreatesCAAndLeaf(t *testing.T) {
	dir := t.TempDir()
	cert, err := Ensure(dir, []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf not set on returned certificate")
	}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		if err := cert.Leaf.VerifyHostname(host); err != nil {
			t.Errorf("leaf does not cover %s: %v", host, err)
		}
	}
	ca := readCert(t, CAPath(dir))
	if !ca.IsCA {
		t.Error("CA certificate is not marked as a CA")
	}
	if ca.Subject.CommonName != caCommonName {
		t.Errorf("CA common name = %q, want %q", ca.Subject.CommonName, caCommonName)
	}
	for name, want := range map[string]os.FileMode{
		caCertFile:     0o644,
		caKeyFile:      0o600,
		serverCertFile: 0o644,
		serverKeyFile:  0o600,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
}

func TestLeafValidityFitsAppleLimit(t *testing.T) {
	dir := t.TempDir()
	cert, err := Ensure(dir, []string{"localhost", "127.0.0.1", "192.0.2.10", "portal.local"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf not set on returned certificate")
	}
	if span := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore); span > 825*24*time.Hour {
		t.Fatalf("leaf validity span %v exceeds Apple's 825-day limit", span)
	}
	if cert.Leaf.Subject.CommonName == "localhost" {
		t.Error("leaf common name should prefer a real host name over localhost")
	}
}

func TestEnsureReusesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost", "127.0.0.1", "portal.local"}
	first, err := Ensure(dir, hosts)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	caBefore := readFile(t, filepath.Join(dir, caCertFile))
	leafBefore := readFile(t, filepath.Join(dir, serverCertFile))

	second, err := Ensure(dir, hosts)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Error("server certificate changed on second Ensure")
	}
	if !bytes.Equal(leafBefore, readFile(t, filepath.Join(dir, serverCertFile))) {
		t.Error("server.crt rewritten on second Ensure")
	}
	if !bytes.Equal(caBefore, readFile(t, filepath.Join(dir, caCertFile))) {
		t.Error("ca.crt rewritten on second Ensure")
	}
}

func TestEnsureRegeneratesLeafForNewHost(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, []string{"localhost"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	caBefore := readFile(t, filepath.Join(dir, caCertFile))
	leafBefore := readFile(t, filepath.Join(dir, serverCertFile))

	cert, err := Ensure(dir, []string{"localhost", "portal.example", "192.0.2.20"})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if bytes.Equal(leafBefore, readFile(t, filepath.Join(dir, serverCertFile))) {
		t.Error("server certificate was not regenerated for a new host")
	}
	for _, host := range []string{"localhost", "portal.example", "192.0.2.20"} {
		if err := cert.Leaf.VerifyHostname(host); err != nil {
			t.Errorf("regenerated leaf does not cover %s: %v", host, err)
		}
	}
	if !bytes.Equal(caBefore, readFile(t, filepath.Join(dir, caCertFile))) {
		t.Error("CA changed when only the leaf needed regeneration")
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, []string{"localhost"}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(first) {
		t.Errorf("fingerprint has unexpected shape: %q", first)
	}
	second, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("second Fingerprint: %v", err)
	}
	if first != second {
		t.Errorf("fingerprint changed: %q != %q", first, second)
	}
}

func TestEnsureReplacesCorruptCA(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost", "127.0.0.1"}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	before, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if err := os.WriteFile(CAPath(dir), []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupt CA: %v", err)
	}
	cert, err := Ensure(dir, hosts)
	if err != nil {
		t.Fatalf("Ensure after corruption: %v", err)
	}
	after, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint after corruption: %v", err)
	}
	if before == after {
		t.Error("CA fingerprint did not change after replacing a corrupt CA")
	}
	if err := cert.Leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("new leaf does not cover localhost: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(readFile(t, path))
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cert
}
