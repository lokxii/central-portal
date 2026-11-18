#!/usr/bin/env bash
# End-to-end test: builds the real server and CLI binaries, starts the server on
# loopback, then exercises the API, the PWA assets, the CLI and persistence.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/portal-it.XXXXXX")
https_port=${HTTPS_PORT:-19443}
http_port=${HTTP_PORT:-19080}
base="https://127.0.0.1:${https_port}"
pid=""
pass=0

say() { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
fail() { printf '\n  FAIL %s\n' "$1" >&2; exit 1; }

assert_eq() {
	[ "$1" = "$2" ] || fail "$3 (got '$1', expected '$2')"
	say "$3"
}

assert_contains() {
	local haystack needle
	haystack=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
	needle=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')
	case "$haystack" in
	*"$needle"*) say "$3" ;;
	*) fail "$3 (missing '$2' in '$1')" ;;
	esac
}

json_get() {
	node --input-type=commonjs -e '
		let s = "";
		process.stdin.setEncoding("utf8");
		process.stdin.on("data", (d) => { s += d; });
		process.stdin.on("end", () => {
			const v = process.argv[1].split(".").reduce((o, k) => (o == null ? o : o[k]), JSON.parse(s));
			process.stdout.write(v == null ? "" : String(v));
		});
	' "$1"
}

png_size() {
	node --input-type=commonjs -e '
		const b = require("fs").readFileSync(process.argv[1]);
		if (b.toString("hex", 0, 8) !== "89504e470d0a1a0a") process.exit(1);
		process.stdout.write(b.readUInt32BE(16) + "x" + b.readUInt32BE(20));
	' "$1"
}

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT

required_tools="go curl openssl sha256sum node base64 seq"
for tool in $required_tools; do
	command -v "$tool" >/dev/null 2>&1 || fail "required tool not found: $tool"
done
say "required tools are available"

start_server() {
	"$tmp/central-portal" -addr "127.0.0.1:${https_port}" -http-addr "127.0.0.1:${http_port}" \
		-data "$tmp/data" -max-clipboard-bytes 1048576 -max-file-bytes 5242880 \
		>>"$tmp/server.log" 2>&1 &
	pid=$!
	local i
	for i in $(seq 1 100); do
		if curl -fsS "http://127.0.0.1:${http_port}/ca.crt" -o "$tmp/ca.crt" 2>/dev/null; then
			for i in $(seq 1 100); do
				if curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" >/dev/null 2>&1; then
					return 0
				fi
				sleep 0.1
			done
		fi
		sleep 0.1
	done
	cat "$tmp/server.log" >&2
	fail "server did not become ready"
}

stop_server() {
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	pid=""
}

echo "== build"
cd "$root"
go build -trimpath -o "$tmp/central-portal" ./cmd/server
go build -trimpath -o "$tmp/cpctl" ./cmd/cpctl
say "server and CLI binaries built"

echo
echo "== startup, CA and TLS"
start_server
say "server started"

openssl x509 -in "$tmp/ca.crt" -noout -text | grep -q 'CA:TRUE' || fail "ca.crt is not a CA certificate"
say "bootstrap CA is a valid CA certificate"

assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard)" "" "empty clipboard is null in status"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get file)" "" "empty file is null in status"
assert_eq "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" "$base/api/clipboard")" "404" "empty clipboard returns 404"

read -r head_code head_size <<<"$(curl -s -o /dev/null -w '%{http_code} %{size_download}' -I --cacert "$tmp/ca.crt" "$base/api/status")"
assert_eq "$head_code" "200" "HEAD /api/status returns 200"
assert_eq "$head_size" "0" "HEAD /api/status has an empty body"

echo
echo "== clipboard registry"
printf 'hello portal\nsecond line' >"$tmp/clip.txt"
"$tmp/cpctl" clip push -text - -server "$base" -ca "$tmp/ca.crt" <"$tmp/clip.txt" >"$tmp/out" 2>"$tmp/err"
assert_contains "$(cat "$tmp/out")" "Uploaded clipboard" "CLI uploads the clipboard"
grep -Eq 'B/s' "$tmp/err" || fail "CLI upload did not print progress on stderr"
say "CLI upload prints progress"

assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/clipboard")" "$(cat "$tmp/clip.txt")" "clipboard bytes roundtrip through the API"
assert_eq "$("$tmp/cpctl" clip pull -stdout -server "$base" -ca "$tmp/ca.crt" 2>/dev/null)" "$(cat "$tmp/clip.txt")" "CLI pulls the clipboard to stdout"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard.size)" "$(wc -c <"$tmp/clip.txt" | tr -d ' ')" "status reports the clipboard size"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type: text/plain; charset=utf-8' --data-binary '' "$base/api/clipboard")
assert_eq "$code" "400" "empty clipboard upload is rejected"

head -c 1048577 /dev/zero >"$tmp/big.bin"
code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type: text/plain; charset=utf-8' --data-binary @"$tmp/big.bin" "$base/api/clipboard")
assert_eq "$code" "413" "oversized clipboard upload is rejected"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/clipboard")" "$(cat "$tmp/clip.txt")" "failed clipboard upload keeps the previous content"

echo
echo "== image clipboard"
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==' | base64 -d >"$tmp/pixel.png"
code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type: image/png' --data-binary @"$tmp/pixel.png" "$base/api/clipboard")
assert_eq "$code" "200" "PNG clipboard upload is accepted"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard.contentType)" "image/png" "image clipboard reports its content type"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard.size)" "$(wc -c <"$tmp/pixel.png" | tr -d ' ')" "image clipboard reports its size"

headers=$(curl -sD - -o "$tmp/pixel.out" --cacert "$tmp/ca.crt" "$base/api/clipboard")
assert_contains "$headers" "Content-Type: image/png" "image download keeps its media type"
assert_eq "$(sha256sum "$tmp/pixel.out" | cut -d' ' -f1)" "$(sha256sum "$tmp/pixel.png" | cut -d' ' -f1)" "image bytes roundtrip through the API"

"$tmp/cpctl" clip pull -stdout -server "$base" -ca "$tmp/ca.crt" >"$tmp/cli.png" 2>/dev/null
assert_eq "$(sha256sum "$tmp/cli.png" | cut -d' ' -f1)" "$(sha256sum "$tmp/pixel.png" | cut -d' ' -f1)" "CLI downloads image bytes to stdout"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type: image/svg+xml' --data-binary '<svg xmlns="http://www.w3.org/2000/svg"/>' "$base/api/clipboard")
assert_eq "$code" "415" "SVG clipboard upload is rejected"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard.contentType)" "image/png" "rejected upload keeps the image clipboard"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type:' --data-binary 'plain default' "$base/api/clipboard")
assert_eq "$code" "200" "upload without a content type defaults to text"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get clipboard.contentType)" "text/plain; charset=utf-8" "default clipboard type is text"

curl -fsS --cacert "$tmp/ca.crt" -X PUT -H 'Content-Type: text/plain; charset=utf-8' --data-binary @"$tmp/clip.txt" -o /dev/null "$base/api/clipboard"
assert_contains "$(curl -sD - -o /dev/null --cacert "$tmp/ca.crt" "$base/api/clipboard")" "text/plain; charset=utf-8" "text clipboard is served as text"

echo
echo "== persistence across a restart"
stop_server
start_server
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/clipboard")" "$(cat "$tmp/clip.txt")" "clipboard survives a server restart"

echo
echo "== file registry"
head -c 2097152 /dev/urandom >"$tmp/report.bin"
sum_before=$(sha256sum "$tmp/report.bin" | cut -d' ' -f1)
"$tmp/cpctl" file push -server "$base" -ca "$tmp/ca.crt" "$tmp/report.bin" >"$tmp/out" 2>"$tmp/err"
assert_contains "$(cat "$tmp/out")" "Uploaded report.bin" "CLI uploads a file"
grep -Eq 'B/s' "$tmp/err" || fail "CLI file upload did not print progress"
say "CLI file upload prints progress"

curl -fsS --cacert "$tmp/ca.crt" -o "$tmp/download.bin" "$base/api/file"
assert_eq "$(sha256sum "$tmp/download.bin" | cut -d' ' -f1)" "$sum_before" "file bytes roundtrip through the API"

range_code=$(curl -s -o "$tmp/range.bin" -w '%{http_code}' --cacert "$tmp/ca.crt" -r 100-199 "$base/api/file")
assert_eq "$range_code" "206" "Range request returns 206"
dd if="$tmp/report.bin" of="$tmp/range.expected" bs=1 skip=100 count=100 2>/dev/null
assert_eq "$(sha256sum "$tmp/range.bin" | cut -d' ' -f1)" "$(sha256sum "$tmp/range.expected" | cut -d' ' -f1)" "Range returns exactly the requested bytes"

mkdir -p "$tmp/downloads"
"$tmp/cpctl" file pull -server "$base" -ca "$tmp/ca.crt" "$tmp/downloads" >"$tmp/out" 2>"$tmp/err"
assert_contains "$(cat "$tmp/out")" "Saved" "CLI saves a file"
assert_eq "$(sha256sum "$tmp/downloads/report.bin" | cut -d' ' -f1)" "$sum_before" "CLI download matches the uploaded file"
[ ! -e "$tmp/downloads/report.bin.part" ] || fail "partial download left behind"
say "CLI leaves no partial file"

unicode_name='naïve 文件.txt'
printf 'unicode payload' >"$tmp/$unicode_name"
"$tmp/cpctl" file push -server "$base" -ca "$tmp/ca.crt" "$tmp/$unicode_name" >/dev/null 2>&1
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get file.name)" "$unicode_name" "unicode file name is preserved in status"
headers=$(curl -sD - -o /dev/null --cacert "$tmp/ca.crt" "$base/api/file")
assert_contains "$headers" "filename*=UTF-8''" "download uses an RFC 5987 file name"
assert_contains "$headers" "application/octet-stream" "download is served as an attachment payload"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT --data-binary 'x' "$base/api/file?name=..%2Fevil.txt")
assert_eq "$code" "200" "traversal-style name is accepted after sanitizing"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get file.name)" "evil.txt" "traversal-style name is reduced to a basename"

assert_contains "$("$tmp/cpctl" status -server "$base" -ca "$tmp/ca.crt")" "evil.txt" "CLI status lists the shared file"

: >"$tmp/empty.bin"
code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X PUT --data-binary @- "$base/api/file?name=empty.bin" <"$tmp/empty.bin")
assert_eq "$code" "200" "zero-byte file is accepted"
assert_eq "$(curl -fsS --cacert "$tmp/ca.crt" "$base/api/status" | json_get file.size)" "0" "zero-byte file reports size 0"

echo
echo "== static PWA assets"
for asset in / /index.html /app.js /style.css /manifest.webmanifest /sw.js \
	/icons/icon-192.png /icons/icon-512.png /icons/icon-maskable-512.png /icons/apple-touch-icon.png; do
	code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" "$base$asset")
	assert_eq "$code" "200" "asset $asset"
done

headers=$(curl -sI --cacert "$tmp/ca.crt" "$base/")
assert_contains "$headers" "Content-Security-Policy" "CSP header is present"
assert_contains "$headers" "X-Content-Type-Options: nosniff" "nosniff header is present"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" "$base/missing.txt")
assert_eq "$code" "404" "unknown static path returns 404"
assert_contains "$(curl -sD - -o /dev/null --cacert "$tmp/ca.crt" "$base/missing.txt")" "Cache-Control: no-store" "404 responses are not cached"

code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$tmp/ca.crt" -X DELETE "$base/api/clipboard")
assert_eq "$code" "405" "unsupported method returns 405"
assert_contains "$(curl -sD - -o /dev/null --cacert "$tmp/ca.crt" -X DELETE "$base/api/clipboard")" "Cache-Control: no-store" "405 responses are not cached"

assert_contains "$(curl -sI --cacert "$tmp/ca.crt" "$base/manifest.webmanifest")" "application/manifest+json" "manifest is served with the PWA media type"

curl -fsS --cacert "$tmp/ca.crt" -o "$tmp/manifest.json" "$base/manifest.webmanifest"
manifest_ok=$(node --input-type=commonjs -e '
	const m = require(process.argv[1]);
	const ok = m.display === "standalone" && m.orientation === "portrait" &&
		Array.isArray(m.icons) && m.icons.length >= 3 &&
		m.icons.some((i) => String(i.purpose).includes("maskable"));
	process.stdout.write(ok ? "ok" : "bad");
' "$tmp/manifest.json")
assert_eq "$manifest_ok" "ok" "manifest declares a standalone portrait PWA with maskable icons"

assert_eq "$(png_size "$root/web/static/icons/icon-192.png")" "192x192" "192px icon is a valid PNG"
assert_eq "$(png_size "$root/web/static/icons/icon-512.png")" "512x512" "512px icon is a valid PNG"
assert_eq "$(png_size "$root/web/static/icons/icon-maskable-512.png")" "512x512" "maskable icon is a valid PNG"
assert_eq "$(png_size "$root/web/static/icons/apple-touch-icon.png")" "180x180" "apple touch icon is a valid PNG"

curl -fsS --cacert "$tmp/ca.crt" -o "$tmp/index.html" "$base/"
grep -q 'manifest.webmanifest' "$tmp/index.html" || fail "index.html does not link the manifest"
say "app shell links the manifest"
if grep -Eo '<script[^>]*>' "$tmp/index.html" | grep -vq 'src='; then
	fail "index.html contains an inline script"
fi
if grep -Eqi '<[^>]+[[:space:]]on[a-z]+[[:space:]]*=' "$tmp/index.html"; then
	fail "index.html contains an inline event handler"
fi
say "app shell has no inline scripts or handlers"

curl -fsS "http://127.0.0.1:${http_port}/" -o "$tmp/onboard.html"
assert_contains "$(cat "$tmp/onboard.html")" "ca.crt" "bootstrap page links the CA certificate"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${http_port}/")
assert_eq "$code" "405" "bootstrap POST / is rejected with 405"
bootstrap_headers=$(curl -sI "http://127.0.0.1:${http_port}/")
assert_contains "$bootstrap_headers" "Referrer-Policy: no-referrer" "bootstrap onboarding sets a referrer policy"
assert_contains "$bootstrap_headers" "Content-Security-Policy" "bootstrap onboarding sets a CSP"

echo
echo "== CLI CA bootstrap and TLS verification"
"$tmp/cpctl" ca -server "$base" -ca-url "http://127.0.0.1:${http_port}/ca.crt" -out "$tmp/fetched-ca.crt" >"$tmp/ca.out" 2>&1
assert_eq "$(sha256sum "$tmp/fetched-ca.crt" | cut -d' ' -f1)" "$(sha256sum "$tmp/ca.crt" | cut -d' ' -f1)" "CLI downloads the same CA"
grep -Eq '([0-9A-F]{2}:){31}[0-9A-F]{2}' "$tmp/ca.out" || fail "CLI did not print the CA fingerprint"
say "CLI prints the CA fingerprint"

if "$tmp/cpctl" status -server "$base" -ca "" >/dev/null 2>&1; then
	fail "CLI trusted an untrusted certificate"
fi
say "CLI refuses an untrusted certificate"

if env -u WAYLAND_DISPLAY "$tmp/cpctl" clip pull -server "$base" -ca "$tmp/ca.crt" >/dev/null 2>"$tmp/wl.err"; then
	fail "clip pull succeeded without a Wayland session"
fi
grep -qi 'wayland' "$tmp/wl.err" || fail "clip pull failure does not mention Wayland"
say "clip pull fails safely without Wayland"

if env -u WAYLAND_DISPLAY "$tmp/cpctl" clip push -server "$base" -ca "$tmp/ca.crt" >/dev/null 2>"$tmp/wl.err"; then
	fail "clip push succeeded without a Wayland session"
fi
grep -qi 'wayland' "$tmp/wl.err" || fail "clip push failure does not mention Wayland"
say "clip push fails safely without Wayland"

echo
echo "integration: ${pass} checks passed"
