#!/bin/sh
# Builds internal/web/static/app.css with the pinned Tailwind CSS standalone
# CLI and the vendored daisyUI plugin. No Node, no npm. Used by `make css`
# and by the CSS stage of the Dockerfile.
set -eu

VERSION=v4.3.3
cd "$(dirname "$0")/.."

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

case "$(uname -s)-$(uname -m)" in
Linux-x86_64) asset=tailwindcss-linux-x64 sum=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a ;;
Linux-aarch64 | Linux-arm64) asset=tailwindcss-linux-arm64 sum=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195 ;;
Darwin-arm64) asset=tailwindcss-macos-arm64 sum=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d ;;
*)
	echo "tailwind.sh: unsupported platform $(uname -s)-$(uname -m)" >&2
	exit 1
	;;
esac

bin="bin/tailwindcss-$VERSION"
if [ ! -x "$bin" ]; then
	mkdir -p bin
	curl -fsSL -o "$bin.download" "https://github.com/tailwindlabs/tailwindcss/releases/download/$VERSION/$asset"
	echo "$sum  $bin.download" | sha256 -c - >/dev/null
	chmod +x "$bin.download"
	mv "$bin.download" "$bin"
fi

(cd internal/web/css && sha256 -c SHA256SUMS >/dev/null)
"$bin" -i internal/web/css/input.css -o internal/web/static/app.css --minify
