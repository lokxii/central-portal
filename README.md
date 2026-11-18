# Central Portal

One shared clipboard (text or an image) and one shared file for every device on
my ZeroTier network. Three small components:

| Path | Component | Stack |
| --- | --- | --- |
| `cmd/server`, `internal/server`, `internal/certs` | HTTPS server | Go, stdlib only |
| `web/` | portrait PWA (dark only) | plain JS + JSDoc, no build step |
| `cmd/cpctl`, `internal/client` | CLI (Wayland clipboard) | Go, stdlib only |

`internal/store` keeps both registries on disk, `internal/model` is the shared
JSON shape, and `API-ENDPOINTS.md` is the contract all three implement.

## Build

```sh
make build          # bin/central-portal and bin/cpctl
make test           # go test ./...
./test/integration.sh   # real binaries + curl against a loopback server
```

Go 1.22+ is required. There are no Go dependencies. For the optional JavaScript
typecheck (`make typecheck`) install the dev-only TypeScript compiler once:

```sh
npm install
```

## Run the server

```sh
./bin/central-portal -data ./data
```

Defaults:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:8443` | HTTPS |
| `-http-addr` | `:8080` | plain HTTP, only serves `ca.crt` + onboarding page |
| `-data` | `data` | registries and certificates |
| `-max-clipboard-bytes` | `16777216` | clipboard upload limit |
| `-max-file-bytes` | `1073741824` | file upload limit |

The server prints the tunnel URL and the CA fingerprint at startup. Open
`https://<portal-ip>:8443` (the address it printed) after trusting the CA below.

## HTTPS without buying a certificate

Public CAs cannot issue certificates for private tunnel IPs, so the server runs
its own local CA. Install that CA once per device and the browser stops showing
warnings — for both the portal and the installed PWA.

1. On the device, open `http://<portal-ip>:8080` (plain HTTP works before the
   CA is trusted) and download `ca.crt`.
2. Install it:
   - **Android:** Settings → Security → Encryption & credentials → Install a
     certificate → CA certificate → pick the downloaded file. Chrome and the
     installed PWA trust it afterwards.
   - **iOS/iPadOS:** open the file, allow the profile, install it in Settings →
     General → VPN & Device Management, then enable full trust in Settings →
     General → About → Certificate Trust Settings.
   - **Fedora:** `sudo trust anchor --store ca.crt`
   - **Debian/Ubuntu:** `sudo cp ca.crt /usr/local/share/ca-certificates/central-portal.crt && sudo update-ca-certificates`
3. Compare the CA fingerprint printed by `cpctl ca` with the fingerprint in the
   server log. They must match.

`ca.key` stays in `data/certs/` and is never served. Anyone who can read it can
mint certificates your devices trust, so keep the data directory private. It is
gitignored: never commit or upload `data/`.

## PWA

After the CA is trusted, open `https://<portal-ip>:8443` and use the browser's
“Add to Home screen”. The app is dark-only, portrait, single page, and caches its
shell for offline use. The clipboard card shows text in an editor or an image
preview, and Paste accepts either from the phone clipboard. It has three
clipboard actions (Paste, Copy, Save) and two file actions (Upload, Download),
with upload progress in the page.

Image paste relies on the browser's clipboard API; Chrome on Android supports
images, while some browsers only expose text. Images can always be received and
Copy converts JPEG/WebP/GIF to PNG locally before putting them on the phone
clipboard.

## CLI on another machine

`bin/cpctl` is a static Go binary for linux/arm64. Put it on `PATH` on the
machine that has the clipboard (cross-compile with `GOOS`/`GOARCH` if needed).

```sh
export CP_SERVER=https://<portal-ip>:8443
cpctl ca                          # fetch CA + print trust instructions
cpctl clip push                   # Wayland clipboard (text or image) -> registry
cpctl clip pull                   # registry -> Wayland clipboard
cpctl file push ./report.pdf      # file -> registry
cpctl file pull                   # registry -> ~/Downloads/report.pdf
cpctl file pull ~/Documents       # or a specific directory/file
cpctl status                      # show both entries
```

Every command accepts `-server URL` and `-ca PATH`, or the `CP_SERVER` and
`CP_CA` environment variables. There is no built-in server address: set
`CP_SERVER` or pass `-server`. The CA defaults to
`~/.config/central-portal/ca.crt` when present, otherwise the system roots.
Transfers print a progress bar on stderr; `clip pull -stdout` keeps stdout clean
for piping.

Clipboard operations are Wayland-only and fail safely: on a machine without
`WAYLAND_DISPLAY` (including a headless host) or without `wl-clipboard`, the CLI
errors before touching the network or any clipboard. Install `wl-clipboard`
(`sudo dnf install wl-clipboard` on Fedora).

## Deploy as a service

```sh
make build
sudo install -d /opt/central-portal/bin
sudo install -m 0755 bin/central-portal /opt/central-portal/bin/
sudo cp deploy/central-portal@.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now central-portal@$USER
journalctl -u central-portal@$USER -f
```

The template unit runs as your user, reads `/opt/central-portal`, and keeps
registries and certificates in `/var/lib/central-portal`. If you already have a
`data/` directory whose CA your devices trust, copy it before the first start:

```sh
sudo install -d -m 0700 /var/lib/central-portal
sudo cp -a data/. /var/lib/central-portal/
sudo chown -R "$USER:$USER" /var/lib/central-portal
```

The service hardens the system with a read-only filesystem except its state
directory.

## Security notes

- There is no authentication: the ZeroTier network is the boundary. Do not
  forward these ports to the internet. On a shared LAN, restrict `:8443` and
  `:8080` to the ZeroTier interface with `firewall-cmd`.
- Uploads are size-limited while streaming; user file names are metadata only
  and are reduced to a basename before storage (they never become paths).
- Both registries are replaced atomically, so a failed upload cannot corrupt
  the previous item.
- Downloads are served as `application/octet-stream` with
  `Content-Disposition: attachment`. The clipboard accepts text or a raster
  image only (PNG/JPEG/WebP/GIF); SVG and markup types are rejected with 415, so
  uploaded content cannot execute in the portal origin. Every response sets
  `nosniff` plus a strict CSP.
- TLS verification is always on in the CLI. `ca.crt` is public; `ca.key` is not.

## Troubleshooting

- **“certificate signed by unknown authority”** — the CA is not installed on
  that device. Run `cpctl ca` and follow the printed instructions.
- **iPhone warns even after installing the CA** — enable full trust in
  Settings → General → About → Certificate Trust Settings. iOS also rejects
  server certificates valid for more than 825 days (including private CAs), so
  the server issues 824-day leaves and renews them automatically 30 days before
  expiry.
- **Clipboard commands fail** — expected on the host and on any non-Wayland
  session. Use `-text` / `-stdout` for testing, or a Wayland desktop for the
  real clipboard.
- **413 upload rejected** — raise `-max-file-bytes` or `-max-clipboard-bytes`.
- **415 unsupported clipboard media type** — the clipboard accepts text or a
  raster image (PNG/JPEG/WebP/GIF) only; markup types such as SVG are refused
  by design.
- **Address already in use** — change `-addr` / `-http-addr` (e.g.
  `-addr :9443 -http-addr :9080`).
- **PWA does not install** — it must be opened over HTTPS with a trusted CA and
  the manifest/service worker reachable; check the browser console.
