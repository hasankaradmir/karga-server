#!/bin/sh
# Installs a karga-server release (Linux) into $KARGA_BIN_DIR (default /usr/local/bin) after
# checking it against the release's checksums.txt, and runs it as a systemd service.
#
#   KARGA_VERSION       tag to install (default: the latest published release)
#   KARGA_BIN_DIR       where to put the binary
#   KARGA_DOWNLOAD_URL  download from here instead of GitHub (a mirror, or file:// for a local build)
#   KARGA_SKIP_SIGNATURE=1  do not verify the signature (a local build has none)
#   KARGA_SYSTEMD=0     install the binary only, without the service
#
# The karga CLI is installed separately: https://github.com/hasankaradmir/karga-cli
set -eu

repo=hasankaradmir/karga-server
bin_dir=${KARGA_BIN_DIR:-/usr/local/bin}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
[ "$os" = linux ] || { echo "karga-server runs on Linux only" >&2; exit 1; }
arch=$(uname -m)
case $arch in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

version=${KARGA_VERSION:-}
if [ -z "$version" ]; then
  [ -z "${KARGA_DOWNLOAD_URL:-}" ] || { echo "set KARGA_VERSION with KARGA_DOWNLOAD_URL" >&2; exit 1; }
  # GitHub redirects /releases/latest to the tag of the latest published release.
  version=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed -n 's#.*/tag/##p')
  [ -n "$version" ] || { echo "no published release found" >&2; exit 1; }
fi
url=${KARGA_DOWNLOAD_URL:-https://github.com/$repo/releases/download/$version}
archive="karga-server_${version#v}_${os}_${arch}.tar.gz"

for f in "$archive" checksums.txt; do
  curl -fsSL -o "$tmp/$f" "$url/$f"
done
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

# checksums.txt is signed by the release workflow (Sigstore, no key): with cosign installed, the
# archive is proven to come from this repository's release, not only to be intact.
if [ "${KARGA_SKIP_SIGNATURE:-0}" != 1 ] && command -v cosign >/dev/null; then
  curl -fsSL -o "$tmp/checksums.txt.sig" "$url/checksums.txt.sig"
  curl -fsSL -o "$tmp/checksums.txt.pem" "$url/checksums.txt.pem"
  cosign verify-blob --certificate "$tmp/checksums.txt.pem" --signature "$tmp/checksums.txt.sig" \
    --certificate-identity-regexp "^https://github.com/$repo/.github/workflows/release.yml@refs/tags/" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$tmp/checksums.txt" >/dev/null 2>&1 \
    || { echo "the signature of checksums.txt does not verify" >&2; exit 1; }
elif [ "${KARGA_SKIP_SIGNATURE:-0}" != 1 ]; then
  echo "note: install cosign to also verify the release signature" >&2
fi

as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }

tar -xzf "$tmp/$archive" -C "$tmp"
changed=1
if [ -f "$bin_dir/karga-server" ] && [ "$(sha256sum "$bin_dir/karga-server" | cut -d' ' -f1)" = "$(sha256sum "$tmp/karga-server" | cut -d' ' -f1)" ]; then
  changed=0
fi
if [ -w "$bin_dir" ]; then mv "$tmp/karga-server" "$bin_dir/"; else as_root mv "$tmp/karga-server" "$bin_dir/"; fi
echo "karga-server $version installed to $bin_dir/karga-server"

# The service: a karga system user in the docker group owns /var/lib/karga; members of the karga
# group may use the CLI.
if [ "${KARGA_SYSTEMD:-1}" = 1 ]; then
  getent group docker >/dev/null || { echo "no docker group: install Docker first" >&2; exit 1; }
  getent group karga >/dev/null || as_root groupadd --system karga
  if id karga >/dev/null 2>&1; then
    as_root usermod -aG docker karga
  else
    as_root useradd --system --gid karga --groups docker --home-dir /var/lib/karga --shell /usr/sbin/nologin karga
  fi
  sed "s#/usr/local/bin/karga-server#$bin_dir/karga-server#" "$tmp/deploy/systemd/karga-server.service" > "$tmp/karga-server.service"
  as_root install -m 0644 "$tmp/karga-server.service" /etc/systemd/system/karga-server.service
  as_root systemctl daemon-reload
  if systemctl is-active --quiet karga-server; then
    # Restarting waits for running operations (up to 5 minutes); only do it for a new binary.
    [ "$changed" = 0 ] || as_root systemctl restart karga-server
    as_root systemctl enable karga-server
  else
    as_root systemctl enable --now karga-server
  fi
  echo "karga-server is running; add users to the karga group to use karga: sudo usermod -aG karga \$USER"
fi
