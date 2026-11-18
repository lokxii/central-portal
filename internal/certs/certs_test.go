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

	cert, err := Ensure(dir, []string{"localhost", "portal.example", "10.1.2.3"})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if bytes.Equal(leafBefore, readFile(t, filepath.Join(dir, serverCertFile))) {
		t.Error("server certificate was not regenerated for a new host")
	}
	for _, host := range []string{"localhost", "portal.example", "10.1.2.3"} {
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

func TestEnsureRejectsCorruptCA(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost", "127.0.0.1"}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	corrupt := []byte("not a certificate")
	if err := os.WriteFile(CAPath(dir), corrupt, 0o644); err != nil {
		t.Fatalf("corrupt CA: %v", err)
	}
	if _, err := Ensure(dir, hosts); err == nil {
		t.Fatal("Ensure succeeded with a corrupt CA, want an error")
	}
	if got := readFile(t, CAPath(dir)); !bytes.Equal(got, corrupt) {
		t.Error("corrupt CA file was replaced")
	}
}

func TestEnsureOneMissingCAFileErrors(t *testing.T) {
	for _, missing := range []string{caCertFile, caKeyFile} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			hosts := []string{"localhost"}
			if _, err := Ensure(dir, hosts); err != nil {
				t.Fatalf("first Ensure: %v", err)
			}
			present := caKeyFile
			if missing == caKeyFile {
				present = caCertFile
			}
			presentBefore := readFile(t, filepath.Join(dir, present))
			path := filepath.Join(dir, missing)
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove %s: %v", missing, err)
			}
			if _, err := Ensure(dir, hosts); err == nil {
				t.Fatalf("Ensure succeeded with %s missing, want an error", missing)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s was recreated", missing)
			}
			if !bytes.Equal(presentBefore, readFile(t, filepath.Join(dir, present))) {
				t.Errorf("%s changed while %s was missing", present, missing)
			}
		})
	}
}

func TestEnsureCreatesMissingCA(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost"}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	for _, name := range []string{caCertFile, caKeyFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
	cert, err := Ensure(dir, hosts)
	if err != nil {
		t.Fatalf("Ensure with both CA files absent: %v", err)
	}
	if err := cert.Leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("new leaf does not cover localhost: %v", err)
	}
	if _, err := Fingerprint(dir); err != nil {
		t.Errorf("Fingerprint after recreation: %v", err)
	}
}

func TestResetAllowsRecreation(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost"}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	before, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if err := Reset(dir); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, name := range []string{caCertFile, caKeyFile, serverCertFile, serverKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Reset", name)
		}
	}
	if err := Reset(dir); err != nil {
		t.Errorf("second Reset: %v", err)
	}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("Ensure after Reset: %v", err)
	}
	after, err := Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint after Reset: %v", err)
	}
	if before == after {
		t.Error("CA fingerprint did not change after Reset")
	}
}

func TestEnsureEnforcesPermissions(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost"}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	for _, name := range []string{caKeyFile, serverKeyFile} {
		if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}
	if _, err := Ensure(dir, hosts); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := dirMode(t, dir); got != 0o700 {
		t.Errorf("directory mode = %o, want 0700", got)
	}
	for _, name := range []string{caKeyFile, serverKeyFile} {
		if got := fileMode(t, filepath.Join(dir, name)); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}
}

func dirMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	return dirMode(t, path)
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
