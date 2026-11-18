# API Endpoints

The portal exposes one HTTP API. All three components (server, browser app,
CLI) are built against this document.

- **HTTPS base URL:** `https://<zerotier-ip>:8443`
- **Bootstrap HTTP base URL:** `http://<zerotier-ip>:8080`
- **Auth:** none. Access control is the ZeroTier network itself. Bind the
  service to the tunnel address (or firewall it) and do not expose it publicly.
- **Version:** v1

## Conventions

- Control endpoints return UTF-8 JSON. Payload endpoints (`/api/clipboard`,
  `/api/file`) return raw bytes.
- API responses carry `Cache-Control: no-store`.
- Errors are `text/plain; charset=utf-8` bodies with a short human-readable
  message, plus a meaningful status code.
- Every upload **replaces** the whole registry. Replacement is atomic: a
  concurrent reader sees either the previous item or the new one, never a
  partial write.
- A failed, interrupted, or oversized upload leaves the previous item intact.
- Size limits are enforced server-side, including for chunked requests without
  `Content-Length`.
- Client-supplied file names are metadata only. The server never uses them as
  filesystem paths; it reduces them to a basename before storing them.
- `HEAD` is accepted wherever `GET` is accepted.
- No CORS headers. The browser app is served from the same origin.

## Data types

`Entry` — metadata of the item currently in a registry:

```json
{
  "size": 1234,
  "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "contentType": "text/plain; charset=utf-8",
  "updatedAt": "2026-11-18T09:12:33.123456789Z"
}
```

- `name` is present only for the file registry (`"name": "notes.txt"`).
- `contentType` is present only for the clipboard registry. Supported values
  are `text/plain; charset=utf-8`, `image/png`, `image/jpeg`, `image/webp` and
  `image/gif`. Images are opaque bytes; the server never transcodes them.
- `size` is bytes.
- `sha256` is the lowercase hex SHA-256 of the stored bytes.
- `updatedAt` is RFC 3339 in UTC.
- An empty registry is represented as `null`, never as an `Entry` with
  `size: 0`. (`size: 0` means a genuinely empty file was uploaded.)

`Status`:

```json
{ "clipboard": Entry | null, "file": Entry | null }
```

## Endpoints

### `GET /api/status`

Returns metadata for both registries. This is the cheap polling endpoint.

- **200**

```json
{
  "clipboard": {
    "size": 42,
    "sha256": "…",
    "contentType": "text/plain; charset=utf-8",
    "updatedAt": "2026-11-18T09:12:33.123456789Z"
  },
  "file": null
}
```

### `GET /api/clipboard`

Downloads the clipboard registry.

- **200** — the stored `Content-Type` (see `PUT` below), raw stored bytes,
  `Content-Length`, `Last-Modified`. Text and raster images are both served
  inline, so an `<img src="/api/clipboard">` works for image entries.
- **404** — clipboard registry is empty.

The server stores the bytes as uploaded. It does not detect encoding, transcode
images, or append a newline.

### `PUT /api/clipboard`

Overwrites the clipboard registry with the request body. The registry holds
either text or one image, never both.

The request `Content-Type` selects the entry type:

| Request `Content-Type` | Stored as |
| --- | --- |
| absent, empty, or `text/plain` (any parameters) | `text/plain; charset=utf-8` |
| `image/png` | `image/png` |
| `image/jpeg` | `image/jpeg` |
| `image/webp` | `image/webp` |
| `image/gif` | `image/gif` |
| anything else (including `image/svg+xml`) | rejected with **415** |

SVG and other active/markup types are rejected deliberately: downloaded
registry content must never be able to execute in the portal origin.

- **Limit:** `-max-clipboard-bytes`, default 16 MiB.
- **200** — `Entry` of the stored clipboard, including `contentType`.
- **400** — empty body.
- **413** — body exceeds the limit; the previous clipboard is unchanged.
- **415** — unsupported media type; the previous clipboard is unchanged.

### `GET /api/file`

Downloads the file registry.

- **200** — `Content-Type: application/octet-stream`, `Content-Length`,
  `Last-Modified`, and
  `Content-Disposition: attachment; filename="…"; filename*=UTF-8''…` when the
  name contains non-ASCII characters.
- Supports `Range` requests (`206`).
- **404** — file registry is empty.

### `PUT /api/file?name=<filename>`

Overwrites the file registry with the request body.

- **Query:** `name` is required. It is sanitized to a basename (both `/` and
  `\` are treated as separators), control characters are rejected, and an empty
  name is rejected.
- **Request:** raw file bytes.
- **Limit:** `-max-file-bytes`, default 1 GiB.
- **200** — `Entry` of the stored file.
- **400** — missing or invalid `name`.
- **413** — body exceeds the limit; the previous file is unchanged.

### `GET /ca.crt`

Serves the PEM-encoded local CA certificate. Available on **both** listeners
(HTTPS `:8443` and bootstrap HTTP `:8080`) so a new device can install trust
before its first HTTPS request.

- **200** — `Content-Type: application/x-x509-ca-cert`, PEM body.

### `GET /` (HTTPS, browser app)

Serves the PWA shell and its assets:

| Path | Purpose |
| --- | --- |
| `/` (and `/index.html`) | app shell |
| `/app.js` | application logic |
| `/style.css` | styles |
| `/manifest.webmanifest` | PWA manifest |
| `/sw.js` | service worker |
| `/icons/icon-192.png` | PWA icon |
| `/icons/icon-512.png` | PWA icon |
| `/icons/icon-maskable-512.png` | maskable PWA icon |
| `/icons/apple-touch-icon.png` | iOS home screen icon |

The clipboard card shows an editable text area for `text/*` entries and an
inline preview for `image/*` entries. Paste accepts text or an image from the
system clipboard; Copy restores whichever entry the registry holds.

- **200** for existing assets, **404** for anything else.
- Static responses are `Cache-Control: no-cache` so an updated shell is picked
  up on the next load; the service worker handles offline use.
- All responses include `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  no-referrer`, and a strict CSP:
  `default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors
  'none'; form-action 'none'; img-src 'self' data:; style-src 'self';
  script-src 'self'; connect-src 'self'; worker-src 'self'; manifest-src
  'self'`.
- Therefore: no inline `<script>`, no inline `style=` attributes, no external
  fonts or CDNs.

### Bootstrap listener (HTTP `:8080`)

Only two routes exist, because HTTPS cannot be used before the CA is trusted:

- `GET /ca.crt` — same certificate as above.
- `GET /{$}` — tiny HTML onboarding page linking to `/ca.crt` and to the HTTPS
  portal.
- Everything else: **404**. The listener never redirects to HTTPS (that would
  just show a certificate warning).

## Error summary

| Status | Meaning |
| --- | --- |
| 400 | Missing/invalid parameter, empty clipboard upload |
| 404 | Registry is empty, or unknown path |
| 405 | Method not allowed for this path |
| 413 | Upload exceeds the configured limit |
| 415 | Unsupported clipboard media type |
| 500 | Server-side I/O failure (check server log) |

## Client responsibilities

- **TLS:** verify the server with the portal CA (`ca.crt`). Never disable
  certificate verification. A client without the CA may report an unknown
  authority error and should point at `ca.crt`.
- **Bootstrap:** fetch `ca.crt` over the HTTP listener only, verify it is a CA
  certificate, and print its SHA-256 fingerprint so the user can compare it
  with the server log.
- **Clipboard:** Wayland only. Refuse to read or write the clipboard when
  `WAYLAND_DISPLAY` is unset or `wl-copy`/`wl-paste` is missing, and do so
  before performing any transfer so the failure is safe. Wayland clipboards can
  hold text or an image; prefer `image/png`, then `image/jpeg`, `image/webp`,
  `image/gif`, then the first `text/*` MIME type. Restore images with
  `wl-copy --type <contentType>`; use `text/plain;charset=utf-8` for text.
- **Images:** send the declared clipboard `Content-Type` on upload and keep the
  server-provided type on download. Never assume text when `contentType` starts
  with `image/`. The browser app may convert a non-PNG image to PNG locally only
  for `ClipboardItem`; the registry itself is never converted.
- **Progress:** derive totals from `Content-Length` (downloads) and file size
  (uploads), and render progress on stderr.
- **Downloads:** write to `<destination>.part`, then rename on success; delete
  the partial file on failure. Default destination is `~/Downloads`, keeping
  the server-provided basename.
- **Polling:** `/api/status` is safe to poll; the browser app does so every
  10 seconds while visible.

## Server configuration

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:8443` | HTTPS listen address |
| `-http-addr` | `:8080` | bootstrap HTTP listen address; empty disables it |
| `-data` | `data` | directory for registries and certificates |
| `-max-clipboard-bytes` | `16777216` | clipboard upload limit |
| `-max-file-bytes` | `1073741824` | file upload limit |

## CLI surface (consumer of this API)

```text
cpctl clip push [-text <text>|-]   # Wayland clipboard (text or image, or -text/stdin) -> registry
cpctl clip pull [-stdout]          # registry -> Wayland clipboard (text or image, or stdout)
cpctl file push <path>             # file -> registry
cpctl file pull [destination]      # registry -> destination (default ~/Downloads)
cpctl status                       # print both registry entries
cpctl ca [-ca-url <url>] [-out <path>]  # install the CA certificate locally
```

`clip push` picks the best available Wayland type (image before text) and sends
its MIME type; `-text` always uploads text. `clip pull` writes the stored type
back to Wayland and prints `Copied <size> <contentType> to the Wayland
clipboard`; `-stdout` writes the raw bytes without adding a newline.

Connection flags on every command: `-server` (or env `CP_SERVER`; required when
neither is set) and `-ca` (or env `CP_CA`, default
`~/.config/central-portal/ca.crt` when that file exists).
