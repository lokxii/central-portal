// Command server runs the central portal: the HTTPS portal and an optional
// plain HTTP bootstrap listener that hands out the local CA certificate.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"central-portal/internal/certs"
	"central-portal/internal/server"
	"central-portal/internal/store"
	"central-portal/web"
)

const onboardingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Central Portal</title>
<style>
body { background: #111; color: #eee; font-family: system-ui, sans-serif; margin: 2rem; line-height: 1.6; }
a { color: #7cc4ff; }
</style>
</head>
<body>
<h1>Central Portal</h1>
<p>1. Install the <a href="/ca.crt">CA certificate</a> on this device.</p>
<p>2. Open <a href="%s">%s</a>.</p>
</body>
</html>
`

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := flag.String("addr", "auto", `HTTPS listen address; "auto" binds the ZeroTier IPv4 address (else loopback), ":8443" or "0.0.0.0:8443" binds all interfaces`)
	httpAddr := flag.String("http-addr", "auto", `bootstrap HTTP listen address; "auto" works like -addr, empty disables`)
	dataDir := flag.String("data", "data", "directory for registries and certificates")
	maxClipboard := flag.Int64("max-clipboard-bytes", 16777216, "clipboard upload limit in bytes")
	maxFile := flag.Int64("max-file-bytes", 1073741824, "file upload limit in bytes")
	rotateCA := flag.Bool("rotate-ca", false, "discard the existing CA and server certificate, then create new ones")
	flag.Parse()

	if *maxClipboard <= 0 || *maxFile <= 0 {
		return errors.New("size limits must be positive")
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	certDir := filepath.Join(*dataDir, "certs")
	if *rotateCA {
		if err := certs.Reset(certDir); err != nil {
			return fmt.Errorf("reset certificates: %w", err)
		}
	}
	ztIP := zerotierIPv4()
	httpsListen, httpsLoopback := server.ResolveListenAddr(*addr, ztIP, "8443")
	if httpsLoopback {
		logger.Warn("no ZeroTier IPv4 address found; binding HTTPS to loopback only",
			"addr", httpsListen, "hint", "pass -addr :8443 to bind all interfaces")
	}
	hosts := localHosts()
	cert, err := certs.Ensure(certDir, hosts)
	if err != nil {
		return err
	}
	fingerprint, err := certs.Fingerprint(certDir)
	if err != nil {
		return err
	}
	webFS, err := fs.Sub(web.Files, "static")
	if err != nil {
		return fmt.Errorf("open embedded frontend: %w", err)
	}

	portal := server.New(server.Config{
		Store:        st,
		Web:          webFS,
		CACertPath:   certs.CAPath(certDir),
		Logger:       logger,
		MaxClipboard: *maxClipboard,
		MaxFile:      *maxFile,
	})
	httpsSrv := &http.Server{
		Handler: portal,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No total ReadTimeout/WriteTimeout: the handler sets per-operation
		// idle deadlines, so large transfers may run as long as they progress.
	}
	httpsLn, err := net.Listen("tcp", httpsListen)
	if err != nil {
		return fmt.Errorf("listen https %s: %w", httpsListen, err)
	}
	defer httpsLn.Close()

	httpsPort := portOf(httpsListen, "8443")
	advertised := advertisedFor(httpsListen, hosts)
	httpsURL := "https://" + net.JoinHostPort(advertised, httpsPort)

	var httpSrv *http.Server
	var httpLn net.Listener
	httpPort := "8080"
	if *httpAddr != "" {
		httpListenAddr, httpLoopback := server.ResolveListenAddr(*httpAddr, ztIP, httpPort)
		if httpLoopback {
			logger.Warn("no ZeroTier IPv4 address found; binding bootstrap HTTP to loopback only",
				"addr", httpListenAddr, "hint", "pass -http-addr :8080 to bind all interfaces")
		}
		httpLn, err = net.Listen("tcp", httpListenAddr)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", httpListenAddr, err)
		}
		defer httpLn.Close()
		httpPort = portOf(httpListenAddr, httpPort)
		httpSrv = &http.Server{
			Handler:           bootstrapHandler(certs.CAPath(certDir), advertised, httpPort, logger),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       time.Minute,
		}
	}

	httpListen := "disabled"
	httpURL := "disabled"
	if httpSrv != nil {
		httpListen = httpLn.Addr().String()
		httpURL = "http://" + net.JoinHostPort(advertised, httpPort)
	}
	logger.Info("central portal started",
		"https_listen", httpsLn.Addr().String(),
		"https_url", httpsURL,
		"http_listen", httpListen,
		"http_url", httpURL,
		"data_dir", *dataDir,
		"ca_cert", certs.CAPath(certDir),
		"ca_fingerprint", fingerprint,
		"max_clipboard_bytes", *maxClipboard,
		"max_file_bytes", *maxFile,
	)

	errc := make(chan error, 2)
	go func() { errc <- httpsSrv.ServeTLS(httpsLn, "", "") }()
	if httpSrv != nil {
		go func() { errc <- httpSrv.Serve(httpLn) }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errc:
		shutdownServers(httpsSrv, httpSrv)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpsSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown https: %w", err)
		}
		if httpSrv != nil {
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown http: %w", err)
			}
		}
		return nil
	}
}

func shutdownServers(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		if srv != nil {
			_ = srv.Shutdown(ctx)
		}
	}
}

// bootstrapHandler serves the CA certificate and a tiny onboarding page. Every
// other path is a 404 and nothing redirects to HTTPS. The onboarding link uses
// the advertised host resolved at startup, never the request Host header.
func bootstrapHandler(caPath, advertisedHost, httpsPort string, logger *slog.Logger) http.Handler {
	if advertisedHost == "" {
		advertisedHost = "localhost"
	}
	onboardingURL := html.EscapeString("https://" + net.JoinHostPort(advertisedHost, httpsPort))
	const bootstrapCSP = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(caPath)
		if err != nil {
			logger.Error("read CA certificate", "error", err)
			http.Error(w, "CA certificate not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", bootstrapCSP)
		fmt.Fprintf(w, onboardingHTML, onboardingURL, onboardingURL)
	})
	// Router-generated 404/405 responses need the same headers as the routes.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", bootstrapCSP)
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

// localHosts lists the names and addresses the server certificate must cover.
func localHosts() []string {
	hosts := make([]string, 0, 8)
	seen := make(map[string]bool)
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	if name, err := os.Hostname(); err == nil {
		add(name)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return hosts
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP
			if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				continue
			}
			add(ip.String())
		}
	}
	return hosts
}

// advertisedFor picks the host to advertise for a concrete listen address: an
// explicit host wins, otherwise fall back to the ZeroTier/LAN address.
func advertisedFor(listen string, hosts []string) string {
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
			return host
		}
	}
	return advertisedHost(hosts)
}

// zerotierIPv4 returns the first IPv4 address of a ZeroTier interface, or "".
func zerotierIPv4() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		if !strings.HasPrefix(iface.Name, "zt") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() {
				return ipNet.IP.String()
			}
		}
	}
	return ""
}

// advertisedHost picks the address to advertise: a ZeroTier IPv4 address when
// the tunnel is up, otherwise the first non-loopback address.
func advertisedHost(hosts []string) string {
	if ip := zerotierIPv4(); ip != "" {
		return ip
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			return host
		}
	}
	return "localhost"
}

// portOf extracts the port from a listen address.
func portOf(addr, fallback string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fallback
	}
	return port
}
