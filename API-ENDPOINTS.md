# API Endpoints

The portal exposes one HTTP API. All three components (server, browser app,
CLI) are built against this document.

- **HTTPS base URL:** `https://<zerotier-ip>:8443`
- **Bootstrap HTTP base URL:** `http://<zerotier-ip>:8080`
- **Auth:** none. Access control is the ZeroTier network itself. The default
  `auto` listen addresses bind the tunnel IPv4 when it is up (loopback
  otherwise); passing a wildcard address such as `:8443` deliberately exposes
  the service on every interface. Do not expose it publicly.
- **Version:** v1

## Conventions

- Control endpoints return UTF-8 JSON. Payload endpoints (`/api/clipboard`,
  `/api/file`) return raw bytes. `GET /api/events` is a long-lived
  `text/event-stream`.
- API responses carry `Cache-Control: no-store`, including error responses
  (`404`, `405`, `413`, and so on). The bootstrap listener sets it on
  `/ca.crt` as well; static PWA assets use `no-cache` instead (see below).
- Errors are `text/plain; charset=utf-8` bodies with a short human-readable
  message, plus a meaningful status code. They also carry
  `X-Content-Type-Options: nosniff` and the same CSP/referrer policy as other
  responses.
- Every upload **replaces** the whole registry. Replacement is atomic: a
  concurrent reader sees either the previous item or the new one, never a
  partial write.
- A failed, interrupted, or oversized upload leaves the previous item intact.
- Size limits are enforced server-side, including for chunked requests without
  `Content-Length`.
- Client-supplied file names are metadata only. The server never uses them as
  filesystem paths; it reduces them to a basename before storing them.
- `HEAD` is accepted wherever `GET` is accepted, with an empty body.
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

### `GET /api/events`

Streams clipboard registry changes as small server-sent events over a
long-lived `text/event-stream` response.

- **200** — the stream stays open until the client disconnects or the server
  stops.
- The first event is the current entry, or `null` when the clipboard registry
  is empty. A client can therefore reconnect without a separate status request.
- Every successful `PUT /api/clipboard` then produces another event carrying
  the new `Entry`.
- Each `data:` line is one JSON object:

```json
{ "kind": "clipboard", "entry": { "size": 42, "sha256": "…", "contentType": "text/plain; charset=utf-8", "updatedAt": "2026-11-18T09:12:33.123456789Z" } }
```

- An empty registry snapshot is `{ "kind": "clipboard", "entry": null }`.
- Comment lines (`: keep-alive`) are sent every 15 seconds and must be ignored.
- No event history is kept and there is no `Last-Event-ID` replay. Reconnect,
  use the snapshot, and reconcile against `/api/status` periodically.
- Slow consumers may miss an update; the next event or a reconnect repairs the
  state. Events are advisory, not a durable log.
- Only clipboard changes are streamed; file registry changes are not.

### `GET /api/clipboard`

Downloads the clipboard registry.

- **200** — the stored `Content-Type` (see `PUT` below), raw stored bytes,
  `Content-Length`, `Last-Modified`. Text and raster images are both served
  inline, so an `<img src="/api/clipboard">` works for image entries.
- **404** — clipboard registry is empty.

The server stores the bytes as uploaded. It does not detect encoding, transcode
images, or append a newline. Text is always stored as
`text/plain; charset=utf-8`, regardless of the charset or other parameters on a
`text/plain` request. A Wayland clipboard may normalize line endings or a
trailing newline before upload, so the stored text can differ subtly from what
was originally on the clipboard.

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
  `Last-Modified`, and a `Content-Disposition: attachment` header with a
  `filename` parameter for ASCII names (quoted when needed). Non-ASCII names
  use the RFC 5987 fallback
  `filename="download"; filename*=UTF-8''<percent-encoded name>` so the header
  itself stays ASCII.
- **206** — a `Range` request was satisfied; the body contains exactly the
  requested bytes.
- **416** — the `Range` is unsatisfiable (for example it starts past the end of
  the file).
- **404** — file registry is empty.

### `PUT /api/file?name=<filename>`

Overwrites the file registry with the request body.

- **Query:** `name` is required. It is sanitized to a basename (both `/` and
  `\` are treated as separators), control characters are rejected, an empty name
  is rejected, and names longer than 200 Unicode runes are rejected.
- **Request:** raw file bytes.
- **Limit:** `-max-file-bytes`, default 1 GiB.
- **200** — `Entry` of the stored file.
- **400** — missing, invalid, or over-long `name`.
- **413** — body exceeds the limit; the previous file is unchanged.

### `GET /ca.crt`

Serves the PEM-encoded local CA certificate. Available on **both** listeners
(HTTPS `:8443` and bootstrap HTTP `:8080`) so a new device can install trust
before its first HTTPS request.

- **200** — `Content-Type: application/x-x509-ca-cert`,
  `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, PEM body.

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
  portal. It sets `Referrer-Policy: no-referrer`, `X-Content-Type-Options:
  nosniff`, and a strict CSP. The page has no scripts.
- Unknown paths: **404**.
- Wrong method on a known path (for example `POST /` or `POST /ca.crt`):
  **405 Method Not Allowed**, with an `Allow: GET, HEAD` header.
- Bootstrap `404`/`405` responses also carry `Cache-Control: no-store`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and the
  bootstrap CSP.
- The listener never redirects to HTTPS (that would just show a certificate
  warning).

## Error summary

| Status | Meaning |
| --- | --- |
| 400 | Missing/invalid parameter, empty clipboard upload |
| 404 | Registry is empty, or unknown path |
| 405 | Method not allowed for this path |
| 413 | Upload exceeds the configured limit |
| 415 | Unsupported clipboard media type |
| 416 | Unsatisfiable `Range` request |
| 500 | Server-side I/O failure (check server log) |

## Client responsibilities

- **TLS:** verify the server with the portal CA (`ca.crt`). Never disable
  certificate verification. A client without the CA may report an unknown
  authority error and should point at `ca.crt`.
- **Bootstrap:** fetch `ca.crt` over the HTTP listener only, verify it is a CA
  certificate, save just that one certificate (reject a response that contains
  multiple certificates), and print its SHA-256 fingerprint so the user can
  compare it with the server log. `-fingerprint` pins the expected value and
  must abort on a mismatch. The default URL is
  `http://<server-host>:8080/ca.crt`; `-ca-url` overrides it.
- **Clipboard:** Wayland only. Refuse to read or write the clipboard when
  `WAYLAND_DISPLAY` is unset or `wl-copy`/`wl-paste` is missing, and do so
  before performing any transfer so the failure is safe. Wayland clipboards can
  hold text or an image; prefer `image/png`, then `image/jpeg`, `image/webp`,
  `image/gif`, then the first `text/*` MIME type. Restore images with
  `wl-copy --type <contentType>`; for text use `text/plain;charset=utf-8`.
  The server canonicalizes every text upload to `text/plain; charset=utf-8`,
  and a Wayland clipboard may normalize line endings or a trailing newline
  before the upload, so do not promise byte-identical text round trips.
- **Images:** send the declared clipboard `Content-Type` on upload and keep the
  server-provided type on download. Never assume text when `contentType` starts
  with `image/`. The browser app may convert a non-PNG image to PNG locally only
  for `ClipboardItem`; the registry itself is never converted.
- **Progress:** derive totals from `Content-Length` (downloads) and file size
  (uploads), render progress on stderr, and bound the wait for response headers
  so an unresponsive peer fails instead of hanging.
- **Downloads:** write to a unique temporary file in the destination directory,
  then rename on success; delete the temporary file on failure. Default
  destination is `~/Downloads`, keeping the server-provided basename. Refuse a
  file download larger than 1 GiB rather than filling the local disk.
- **Sync:** `cpctl clip sync` watches the Wayland clipboard and streams
  `/api/events`, writing remote changes locally and uploading local changes.
  It sends the entry's `sha256` back on upload and ignores an event whose hash
  matches the clipboard it just wrote, so one device does not echo its own
  change. On startup the server entry wins when the registry is non-empty; an
  empty registry adopts the local clipboard. Reconnect with backoff, re-read the
  snapshot, and reconcile with `/api/status` periodically in case an event was
  missed.
- **Polling:** `/api/status` is safe to poll; the browser app does so every
  10 seconds while visible. The CLI also reconciles every 30 seconds by default
  as a fallback to the event stream.

## Server configuration

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `auto` | HTTPS listen address; `auto` = ZeroTier IPv4 if present, else loopback |
| `-http-addr` | `auto` | bootstrap HTTP listen address; empty disables it |
| `-data` | `data` | directory for registries and certificates |
| `-rotate-ca` | `false` | replace the local CA and issue a new server certificate |
| `-max-clipboard-bytes` | `16777216` | clipboard upload limit |
| `-max-file-bytes` | `1073741824` | file upload limit |

`auto` never binds all interfaces. Passing an explicit wildcard address
(`-addr :8443`, `-addr 0.0.0.0:8443`) is the deliberate escape hatch for hosts
that should listen beyond ZeroTier and loopback. `certs.Ensure` never replaces an
existing CA silently; use `-rotate-ca` or remove `data/certs/ca.*` to recover
from a corrupt CA.

## CLI surface (consumer of this API)

```text
cpctl clip sync [-interval <duration>] [-reconcile <duration>] [-once]
                                   # keep Wayland clipboard and registry in step
cpctl file push <path>             # file -> registry
cpctl file pull [destination]      # registry -> destination (default ~/Downloads)
cpctl status                       # print both registry entries
cpctl ca [-ca-url <url>] [-out <path>] [-fingerprint <AA:BB:…>]
```

`clip sync` runs until interrupted. It polls the local clipboard every second
(`-interval`), receives server changes over `/api/events`, and reconciles with
`/api/status` every 30 seconds (`-reconcile`) in case an event was missed. It
picks the best available Wayland type (image before text) when uploading and
writes the stored type back when downloading. `-once` performs one
reconciliation and exits; on an empty registry it uploads the local clipboard,
and otherwise it downloads the server entry.

`cpctl ca` fetches over plain HTTP, defaulting to
`http://<server-host>:8080/ca.crt` unless `-ca-url` is supplied. It saves only
the single CA certificate it downloaded and rejects multi-certificate bundles,
then prints the SHA-256 fingerprint. `-fingerprint` pins that value and fails
the command on a mismatch; use it to compare with the server log before trusting
the CA.

Connection flags on every command: `-server` (env `CP_SERVER`; there is no
built-in address, so one of the two is required) and `-ca` (env `CP_CA`,
default `~/.config/central-portal/ca.crt` when that file exists).
