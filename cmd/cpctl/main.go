// Command cpctl is the command line client for the central portal. It shares
// the Wayland clipboard and one file with the other devices on the tunnel.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"central-portal/internal/client"
	"central-portal/internal/model"
	"central-portal/internal/progress"
)

var (
	errUsage = errors.New("usage")
	errHelp  = errors.New("help requested")
)

const (
	usageClipPush = "usage: cpctl clip push [-text <text>|-] [-server URL] [-ca PATH]\n"
	usageClipPull = "usage: cpctl clip pull [-stdout] [-server URL] [-ca PATH]\n"
	usageFilePush = "usage: cpctl file push <path> [-server URL] [-ca PATH]\n"
	usageFilePull = "usage: cpctl file pull [destination] [-server URL] [-ca PATH]\n"
	usageStatus   = "usage: cpctl status [-server URL] [-ca PATH]\n"
	usageCA       = "usage: cpctl ca [-ca-url URL] [-out PATH] [-server URL] [-ca PATH]\n"
)

var usageText = `cpctl is the command line client for the central portal.

Usage:
  cpctl clip push [-text <text>|-] [-server URL] [-ca PATH]
  cpctl clip pull [-stdout] [-server URL] [-ca PATH]
  cpctl file push <path> [-server URL] [-ca PATH]
  cpctl file pull [destination] [-server URL] [-ca PATH]
  cpctl status [-server URL] [-ca PATH]
  cpctl ca [-ca-url URL] [-out PATH] [-server URL] [-ca PATH]

The clipboard commands use the Wayland clipboard and need wl-clipboard
(WAYLAND_DISPLAY must be set). -server defaults to $CP_SERVER and is required
when that variable is unset. -ca defaults to $CP_CA or
~/.config/central-portal/ca.crt when that file exists, and otherwise to the
system roots.
`

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout)
	switch {
	case err == nil, errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errUsage):
		io.WriteString(os.Stderr, usageText)
		return 2
	case ctx.Err() != nil:
		return 130
	default:
		fmt.Fprintf(os.Stderr, "cpctl: %v\n", err)
		return 1
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	if isHelp(args[0]) {
		io.WriteString(stdout, usageText)
		return errHelp
	}
	switch args[0] {
	case "clip":
		return runClip(ctx, args[1:], stdout)
	case "file":
		return runFile(ctx, args[1:], stdout)
	case "status":
		return statusCommand(ctx, args[1:], stdout)
	case "ca":
		return caCommand(ctx, args[1:], stdout)
	default:
		return errUsage
	}
}

func runClip(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "push":
		return clipPush(ctx, args[1:], stdout)
	case "pull":
		return clipPull(ctx, args[1:], stdout)
	}
	if isHelp(args[0]) {
		io.WriteString(stdout, usageText)
		return errHelp
	}
	return errUsage
}

func runFile(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "push":
		return filePush(ctx, args[1:], stdout)
	case "pull":
		return filePull(ctx, args[1:], stdout)
	}
	if isHelp(args[0]) {
		io.WriteString(stdout, usageText)
		return errHelp
	}
	return errUsage
}

func clipPush(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl clip push", flag.ContinueOnError)
	text := fs.String("text", "", `text to upload; "-" reads standard input`)
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	pos, err := parseFlags(fs, args, stdout, usageClipPush)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errUsage
	}

	var data []byte
	contentType := model.ContentTypeText
	switch {
	case flagWasSet(fs, "text") && *text == "-":
		data, err = readStdin(ctx)
	case flagWasSet(fs, "text"):
		data = []byte(*text)
	default:
		data, contentType, err = readWaylandClipboard(ctx)
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty content: nothing to upload")
	}

	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}
	bar := progress.New("clipboard", int64(len(data)))
	defer bar.Done()
	if _, err := c.PutClipboard(ctx, data, contentType, bar); err != nil {
		return err
	}
	bar.Done()
	fmt.Fprintf(stdout, "Uploaded clipboard (%s)\n", progress.HumanSize(int64(len(data))))
	return nil
}

func clipPull(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl clip pull", flag.ContinueOnError)
	toStdout := fs.Bool("stdout", false, "write the clipboard bytes to standard output")
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	pos, err := parseFlags(fs, args, stdout, usageClipPull)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errUsage
	}

	// Fail before any transfer when the clipboard cannot be written.
	if !*toStdout {
		if err := requireWayland("wl-copy"); err != nil {
			return err
		}
	}

	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}
	var total int64
	if st, statusErr := c.Status(ctx); statusErr == nil && st.Clipboard != nil {
		total = st.Clipboard.Size
	}
	bar := progress.New("clipboard", total)
	defer bar.Done()
	data, contentType, err := c.GetClipboard(ctx, bar)
	if err != nil {
		if errors.Is(err, client.ErrEmpty) {
			return errors.New("clipboard registry is empty")
		}
		return err
	}
	bar.Done()

	if *toStdout {
		_, err := stdout.Write(data)
		return err
	}
	if err := writeWaylandClipboard(ctx, data, contentType); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Copied %s %s to the Wayland clipboard\n",
		progress.HumanSize(int64(len(data))), contentType)
	return nil
}

func filePush(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl file push", flag.ContinueOnError)
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	pos, err := parseFlags(fs, args, stdout, usageFilePush)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}

	path := pos[0]
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}

	name := filepath.Base(path)
	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}
	bar := progress.New(name, fi.Size())
	defer bar.Done()
	entry, err := c.PutFile(ctx, name, f, fi.Size(), bar)
	if err != nil {
		return err
	}
	bar.Done()
	if entry.Name != "" {
		name = entry.Name
	}
	fmt.Fprintf(stdout, "Uploaded %s (%s)\n", name, progress.HumanSize(fi.Size()))
	return nil
}

func filePull(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl file pull", flag.ContinueOnError)
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	pos, err := parseFlags(fs, args, stdout, usageFilePull)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errUsage
	}
	destination := ""
	if len(pos) == 1 {
		destination = pos[0]
	}

	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if st.File == nil {
		return errors.New("file registry is empty")
	}

	name := safeBaseName(st.File.Name)
	dest, err := resolveDestination(destination, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", part, err)
	}
	saved := false
	defer func() {
		if !saved {
			f.Close()
			os.Remove(part)
		}
	}()

	bar := progress.New(name, st.File.Size)
	defer bar.Done()
	n, err := c.GetFile(ctx, io.MultiWriter(f, bar))
	if err != nil {
		if errors.Is(err, client.ErrEmpty) {
			return errors.New("file registry is empty")
		}
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return err
	}
	saved = true
	bar.Done()
	fmt.Fprintf(stdout, "Saved %s (%s)\n", dest, progress.HumanSize(n))
	return nil
}

func statusCommand(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl status", flag.ContinueOnError)
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	pos, err := parseFlags(fs, args, stdout, usageStatus)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errUsage
	}

	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%-10s %s\n", "clipboard", describe(st.Clipboard, false))
	fmt.Fprintf(stdout, "%-10s %s\n", "file", describe(st.File, true))
	return nil
}

func caCommand(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl ca", flag.ContinueOnError)
	caURL := fs.String("ca-url", "", "URL of the CA certificate; default http://<server-host>:8080/ca.crt")
	out := fs.String("out", filepath.Join(userConfigDir(), "ca.crt"), "where to save the certificate")
	server := fs.String("server", defaultServer(), "portal base URL, used to derive -ca-url")
	fs.String("ca", defaultCAFile(), "portal CA certificate (not used by this command)")
	pos, err := parseFlags(fs, args, stdout, usageCA)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errUsage
	}
	if *caURL == "" && *server == "" {
		return errors.New("no server configured: pass -server URL, -ca-url URL, or set CP_SERVER")
	}

	rawURL := *caURL
	if rawURL == "" {
		rawURL, err = bootstrapCAURL(*server)
		if err != nil {
			return err
		}
	}
	body, err := fetchCA(ctx, rawURL)
	if err != nil {
		return err
	}
	certs, err := parseCertificates(body)
	if err != nil {
		return err
	}
	var caCert *x509.Certificate
	for _, cert := range certs {
		if cert.IsCA {
			caCert = cert
			break
		}
	}
	if caCert == nil {
		return errors.New("downloaded certificate is not a CA certificate")
	}
	if err := writeFileAtomic(*out, encodeCertificates(certs)); err != nil {
		return fmt.Errorf("save CA certificate: %w", err)
	}

	fmt.Fprintf(stdout, "SHA-256 fingerprint: %s\n", fingerprint(caCert))
	fmt.Fprintf(stdout, "Saved %s\n", *out)
	fmt.Fprintf(stdout, "\nTrust it on each device:\n\n")
	fmt.Fprintf(stdout, "  Fedora:\n    sudo trust anchor --store %s\n\n", *out)
	fmt.Fprintf(stdout, "  Debian/Ubuntu:\n    sudo cp %s /usr/local/share/ca-certificates/central-portal.crt\n    sudo update-ca-certificates\n\n", *out)
	fmt.Fprintf(stdout, "  Android:\n    Settings → Security → Install a certificate → CA certificate\n    (choose the downloaded ca.crt)\n\n")
	fmt.Fprintf(stdout, "  iOS:\n    Install the profile, then enable full trust in\n    Settings → General → About → Certificate Trust Settings\n")
	return nil
}

// parseFlags parses flags and positional arguments, allowing them to be
// interleaved (for example "cpctl file push x -server URL"). It returns the
// positional arguments.
func parseFlags(fs *flag.FlagSet, args []string, stdout io.Writer, usage string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				io.WriteString(stdout, usage)
				return nil, errHelp
			}
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func readStdin(ctx context.Context) ([]byte, error) {
	const limit = 16 << 20
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, limit+1))
		done <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("read standard input: %w", r.err)
		}
		if len(r.data) > limit {
			return nil, fmt.Errorf("standard input is larger than %d MiB", limit>>20)
		}
		return r.data, nil
	}
}

// resolveDestination turns the optional command argument into a file path.
// A missing destination means ~/Downloads; a directory or trailing slash means
// "join the server's file name".
func resolveDestination(arg, name string) (string, error) {
	if arg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		arg = filepath.Join(home, "Downloads")
	}
	isDir := strings.HasSuffix(arg, "/") || strings.HasSuffix(arg, string(filepath.Separator))
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		isDir = true
	}
	if isDir {
		return filepath.Join(arg, name), nil
	}
	return arg, nil
}

// safeBaseName reduces a server-provided name to a harmless basename. The name
// is metadata only, but the client still never trusts it as a path.
func safeBaseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "download"
	}
	return name
}

func describe(entry *model.Entry, withName bool) string {
	if entry == nil {
		return "empty"
	}
	parts := make([]string, 0, 4)
	if withName && entry.Name != "" {
		parts = append(parts, entry.Name)
	}
	// Images are worth naming; text is the default and stays short.
	if strings.HasPrefix(entry.ContentType, "image/") {
		parts = append(parts, entry.ContentType)
	}
	parts = append(parts, progress.HumanSize(entry.Size))
	if !entry.UpdatedAt.IsZero() {
		parts = append(parts, relTime(entry.UpdatedAt))
	}
	return strings.Join(parts, " · ")
}

// relTime renders a timestamp as "just now", "5m ago", "3h ago" or "2d ago".
func relTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func isHelp(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

// defaultServer has no built-in address: deployments differ and a hardcoded
// host would be wrong (and private) for everyone else.
func defaultServer() string {
	return os.Getenv("CP_SERVER")
}

func newClient(server, ca string) (*client.Client, error) {
	if strings.TrimSpace(server) == "" {
		return nil, errors.New("no server configured: pass -server URL or set CP_SERVER")
	}
	return client.New(server, ca)
}

// defaultCAFile is the CA the portal's own certificate is checked against. An
// empty result means the system roots are used.
func defaultCAFile() string {
	if p := os.Getenv("CP_CA"); p != "" {
		return p
	}
	p := filepath.Join(userConfigDir(), "ca.crt")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func userConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "central-portal")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "central-portal")
	}
	return "central-portal"
}

// bootstrapCAURL derives the plain-HTTP bootstrap URL from the portal URL.
func bootstrapCAURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid server URL %q", server)
	}
	return "http://" + net.JoinHostPort(u.Hostname(), "8080") + "/ca.crt", nil
}

const maxCABytes = 1 << 20

// fetchCA downloads the certificate over the bootstrap listener. Plain HTTP is
// expected because nothing can be trusted before the CA is installed.
func fetchCA(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid CA URL %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid CA URL %q", rawURL)
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("download %s: %s", rawURL, msg)
	}
	if resp.ContentLength > maxCABytes {
		return nil, fmt.Errorf("download %s: certificate is larger than 1 MiB", rawURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCABytes+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if len(body) > maxCABytes {
		return nil, fmt.Errorf("download %s: certificate is larger than 1 MiB", rawURL)
	}
	return body, nil
}

func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("downloaded file contains no PEM certificates")
	}
	return certs, nil
}

func encodeCertificates(certs []*x509.Certificate) []byte {
	var out []byte
	for _, cert := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	return out
}

// fingerprint returns the SHA-256 of the DER certificate as uppercase,
// colon-separated hex.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ca-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
