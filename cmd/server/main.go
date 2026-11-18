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
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	httpAddr := flag.String("http-addr", ":8080", "bootstrap HTTP listen address (empty disables)")
	dataDir := flag.String("data", "data", "directory for registries and certificates")
	maxClipboard := flag.Int64("max-clipboard-bytes", 16777216, "clipboard upload limit in bytes")
	maxFile := flag.Int64("max-file-bytes", 1073741824, "file upload limit in bytes")
	flag.Parse()

	if *maxClipboard <= 0 || *maxFile <= 0 {
		return errors.New("size limits must be positive")
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	certDir := filepath.Join(*dataDir, "certs")
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
		// No WriteTimeout: file downloads may take a long time.
	}
	httpsLn, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen https %s: %w", *addr, err)
	}
	defer httpsLn.Close()

	httpsPort := portOf(*addr, "8443")
	var httpSrv *http.Server
	var httpLn net.Listener
	if *httpAddr != "" {
		httpLn, err = net.Listen("tcp", *httpAddr)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", *httpAddr, err)
		}
		defer httpLn.Close()
		httpSrv = &http.Server{
			Handler:           bootstrapHandler(certs.CAPath(certDir), httpsPort, logger),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       time.Minute,
		}
	}

	advertised := ""
	if host, _, err := net.SplitHostPort(*addr); err == nil {
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
			advertised = host
		}
	}
	if advertised == "" {
		advertised = advertisedHost(hosts)
	}
	httpsURL := "https://" + net.JoinHostPort(advertised, httpsPort)
	httpListen := "disabled"
	httpURL := "disabled"
	if httpSrv != nil {
		httpListen = httpLn.Addr().String()
		httpURL = "http://" + net.JoinHostPort(advertised, portOf(*httpAddr, "8080"))
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
// other path is a 404 and nothing redirects to HTTPS.
func bootstrapHandler(caPath, httpsPort string, logger *slog.Logger) http.Handler {
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
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" {
			host = "localhost"
		}
		url := html.EscapeString("https://" + net.JoinHostPort(host, httpsPort))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		fmt.Fprintf(w, onboardingHTML, url, url)
	})
	return mux
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

// advertisedHost picks the address to advertise: a ZeroTier IPv4 address when
// the tunnel is up, otherwise the first non-loopback address.
func advertisedHost(hosts []string) string {
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range interfaces {
			if !strings.HasPrefix(iface.Name, "zt") {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					return ipNet.IP.String()
				}
			}
		}
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
