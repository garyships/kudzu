#!/bin/sh
# Install the latest kudzu release.
#
#   curl -fsSL https://raw.githubusercontent.com/garyships/kudzu/main/install.sh | sh
#
# Set KUDZU_INSTALL_DIR to choose where the binary goes
# (default: /usr/local/bin if writable, else ~/.local/bin).
set -eu

repo="garyships/kudzu"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux | darwin) ;;
  *) echo "kudzu: unsupported OS '$os'; download a build from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "kudzu: unsupported CPU '$arch'; try: go install github.com/$repo/cmd/kudzu@latest" >&2; exit 1 ;;
esac

dir="${KUDZU_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
asset="kudzu_${os}_${arch}.tar.gz"
base="${KUDZU_RELEASE_URL:-https://github.com/$repo/releases/latest/download}"
echo "downloading $base/$asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

want=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "kudzu: checksum mismatch for $asset" >&2
  exit 1
fi

tar -xzf "$tmp/$asset" -C "$tmp" kudzu
install -m 0755 "$tmp/kudzu" "$dir/kudzu"
echo "installed $("$dir/kudzu" --version) to $dir/kudzu"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on your PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
echo "next: cd into any git repo and run 'kudzu up'"
