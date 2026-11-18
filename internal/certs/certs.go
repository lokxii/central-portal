// Package certs creates and maintains the local certificate authority and the
// server certificate used for HTTPS.
//
// Everything lives in one directory: the CA certificate and key, plus the leaf
// certificate and key presented by the server. Files are written atomically.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Certificate file names inside the certificate directory.
const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
)

const (
	caCommonName = "Central Portal Local CA"
	orgName      = "Central Portal"

	caValidityYears = 10
	// Apple rejects TLS server certificates whose NotBefore..NotAfter span is
	// longer than 825 days, even when the issuing root was installed manually.
	// Stay a day under so the span itself (not just NotAfter) is safely inside
	// the limit.
	leafValidity = 824 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour
	clockSkew    = time.Hour
)

// authority is a loaded CA certificate with its private key.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// CAPath returns the path of the CA certificate inside dir.
func CAPath(dir string) string {
	return filepath.Join(dir, caCertFile)
}

// Fingerprint returns the SHA-256 fingerprint of the CA certificate in dir as
// uppercase hexadecimal bytes separated by colons.
func Fingerprint(dir string) (string, error) {
	data, err := os.ReadFile(CAPath(dir))
	if err != nil {
		return "", fmt.Errorf("read CA certificate: %w", err)
	}
	cert, err := parseCert(data)
	if err != nil {
		return "", fmt.Errorf("parse CA certificate: %w", err)
	}
	return FormatFingerprint(cert), nil
}

// FormatFingerprint returns the SHA-256 of the certificate's DER encoding as
// uppercase hexadecimal bytes separated by colons.
func FormatFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// Ensure makes sure dir holds a usable CA and a server certificate covering
// every host, creating them when absent, and returns the server certificate for
// use by a TLS listener.
//
// An existing CA is never replaced implicitly: a missing, unreadable or invalid
// CA file is reported as an error so that a broken installation cannot silently
// invalidate the trust every device placed in the old CA. Call Reset to rotate
// the CA deliberately.
func Ensure(dir string, hosts []string) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate directory: %w", err)
	}
	if err := enforcePermissions(dir); err != nil {
		return tls.Certificate{}, err
	}
	haveCert, err := fileExists(CAPath(dir))
	if err != nil {
		return tls.Certificate{}, err
	}
	haveKey, err := fileExists(filepath.Join(dir, caKeyFile))
	if err != nil {
		return tls.Certificate{}, err
	}

	var ca *authority
	switch {
	case !haveCert && !haveKey:
		ca, err = createCA(dir)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("create CA: %w", err)
		}
	case haveCert != haveKey:
		present, missing := caCertFile, caKeyFile
		if haveKey {
			present, missing = caKeyFile, caCertFile
		}
		return tls.Certificate{}, fmt.Errorf(
			"certificate directory %s is incomplete: %s exists but %s is missing; restore both files or rotate the CA explicitly (server -rotate-ca)",
			dir, present, missing)
	default:
		ca, err = loadCA(dir)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf(
				"load CA from %s: %w; restore %s and %s or rotate the CA explicitly (server -rotate-ca)",
				dir, err, caCertFile, caKeyFile)
		}
	}
	hosts = uniqueHosts(hosts)
	if cert, ok := loadLeaf(dir, ca, hosts, time.Now()); ok {
		return cert, nil
	}
	cert, err := createLeaf(dir, ca, hosts)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create server certificate: %w", err)
	}
	return cert, nil
}

// Reset removes the CA and server certificate files so that Ensure can create a
// new CA. Missing files are ignored. It is deliberately explicit because Ensure
// never replaces an existing CA on its own.
func Reset(dir string) error {
	for _, name := range []string{caCertFile, caKeyFile, serverCertFile, serverKeyFile} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// fileExists reports whether path exists. Errors other than "not exist" are
// returned so that a transient stat failure is not mistaken for an absent file.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
}

// enforcePermissions keeps the certificate directory private and the private
// keys readable only by their owner.
func enforcePermissions(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict certificate directory %s to 0700: %w", dir, err)
	}
	for _, name := range []string{caKeyFile, serverKeyFile} {
		path := filepath.Join(dir, name)
		if err := os.Chmod(path, 0o600); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("restrict private key %s to 0600: %w", path, err)
		}
	}
	return nil
}

// loadCA reads the CA files and checks that they are usable. Any problem is
// reported as an error; the caller must not replace the CA implicitly.
func loadCA(dir string) (*authority, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	switch {
	case !cert.IsCA:
		return nil, errors.New("certificate is not a CA")
	case cert.KeyUsage&x509.KeyUsageCertSign == 0:
		return nil, errors.New("CA cannot sign certificates")
	case !now.Before(cert.NotAfter.Add(-renewBefore)):
		return nil, errors.New("CA certificate is expiring")
	case !key.PublicKey.Equal(cert.PublicKey):
		return nil, errors.New("CA key does not match certificate")
	}
	return &authority{cert: cert, key: key}, nil
}

// loadLeaf returns the existing server certificate when it is still valid
// against the CA, not about to expire, and covers every host.
func loadLeaf(dir string, ca *authority, hosts []string, now time.Time) (tls.Certificate, bool) {
	certPEM, err := os.ReadFile(filepath.Join(dir, serverCertFile))
	if err != nil {
		return tls.Certificate{}, false
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, serverKeyFile))
	if err != nil {
		return tls.Certificate{}, false
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if !leaf.NotAfter.After(now.Add(renewBefore)) {
		return tls.Certificate{}, false
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > leafValidity {
		// Too long for iOS/macOS; replace it while keeping the same CA.
		return tls.Certificate{}, false
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return tls.Certificate{}, false
	}
	for _, host := range hosts {
		if err := leaf.VerifyHostname(host); err != nil {
			return tls.Certificate{}, false
		}
	}
	pair.Leaf = leaf
	return pair, true
}

// createCA generates and stores a new CA certificate and key.
func createCA(dir string) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{orgName}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.AddDate(caValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("sign CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}
	if err := writeFile(filepath.Join(dir, caCertFile), certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write CA certificate: %w", err)
	}
	return &authority{cert: cert, key: key}, nil
}

// createLeaf generates and stores a server certificate for hosts.
func createLeaf(dir string, ca *authority, hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	notBefore := now.Add(-clockSkew)
	notAfter := notBefore.Add(leafValidity)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	commonName := "Central Portal"
	for _, host := range hosts {
		if net.ParseIP(host) == nil && !strings.EqualFold(host, "localhost") {
			commonName = host
			break
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sign server certificate: %w", err)
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(filepath.Join(dir, serverKeyFile), keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server key: %w", err)
	}
	if err := writeFile(filepath.Join(dir, serverCertFile), certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server certificate: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil {
		pair.Leaf = leaf
	}
	return pair, nil
}

func parseCert(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid key PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not ECDSA")
		}
		return key, nil
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid private key")
	}
	return key, nil
}

func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

// uniqueHosts removes empty and duplicate hosts while keeping their order.
func uniqueHosts(hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

// writeFile atomically replaces path with data and syncs the directory.
func writeFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
