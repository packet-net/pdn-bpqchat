#!/usr/bin/env bash
#
# build-deb.sh — cross-compile the pdn-bpqchat static binary for one Go arch and package it as a
# Debian .deb. Used locally and by .github/workflows/release.yml.
#
#   scripts/build-deb.sh <goarch> <version>
#   e.g. scripts/build-deb.sh arm64 0.0.1
#
# Go's CGO-free cross-compile (GOARCH) builds every arch on one self-hosted x64 runner — no
# arch-native machine, no cross C-toolchain, no bundled runtime (the pure-Go SQLite driver is already
# linked into the binary). Produces artifacts/pdn-bpqchat_<version>_<arch>.deb.
#
# The .deb carries exactly the CODE:
#   usr/share/packetnet/apps/bpqchat/pdn-bpqchat   (the single static binary, 0755)
#   usr/share/packetnet/apps/bpqchat/pdn-app.yaml  (the app manifest, copied from repo root, 0644)
# Runtime STATE (bpqchat.db, chat.yaml) lives in /var/lib/packetnet/apps/bpqchat and is NEVER shipped.
set -euo pipefail

goarch="${1:?usage: build-deb.sh <goarch> <version>  (goarch: amd64|arm64|arm)}"
version="${2:?usage: build-deb.sh <goarch> <version>}"

# Map Go GOARCH → Debian arch. armhf is ARMv7 hard-float → GOARCH=arm with GOARM=7.
goarm=""
case "$goarch" in
  amd64) arch=amd64 ;;
  arm64) arch=arm64 ;;
  arm)   arch=armhf; goarm=7 ;;
  *) echo "unknown goarch: $goarch (want amd64 | arm64 | arm)" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
stage="$root/artifacts/deb/$arch"
out="$root/artifacts/pdn-bpqchat_${version}_${arch}.deb"
bin="$root/artifacts/bin/pdn-bpqchat-$arch"

echo "==> build static binary for $goarch (-> $arch)"
mkdir -p "$(dirname "$bin")"
# CGO_ENABLED=0: a fully static binary, no libc dependency — drops on any glibc/musl host.
# -trimpath + -s -w: reproducible-ish, no debug/symbol bloat. main.version stamps the build.
# env carries the assignments so the conditional GOARM (only set for armhf) is honoured even though
# it comes from a parameter expansion (a bare VAR=val prefix from an expansion isn't treated as an
# assignment by the shell).
env CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" ${goarm:+GOARM="$goarm"} \
  go build -trimpath -ldflags "-s -w -X main.version=${version}" \
  -o "$bin" "$root/cmd/pdn-bpqchat"
[ -f "$bin" ] || { echo "ERROR: binary $bin was not produced" >&2; exit 1; }

echo "==> stage .deb tree for $arch"
rm -rf "$stage"
install -d "$stage/usr/share/packetnet/apps/bpqchat" "$stage/DEBIAN"
install -m 0755 "$bin" "$stage/usr/share/packetnet/apps/bpqchat/pdn-bpqchat"
# Stamp the release version into the manifest so the shipped pdn-app.yaml matches
# the binary/control/release (the repo copy carries a dev default).
sed "s/^version: .*/version: \"$version\"/" "$root/pdn-app.yaml" \
  > "$stage/usr/share/packetnet/apps/bpqchat/pdn-app.yaml"
chmod 0644 "$stage/usr/share/packetnet/apps/bpqchat/pdn-app.yaml"

# Installed-Size (KiB) — apt shows it; dpkg-deb does not compute it for us.
size_kib="$(du -k -s "$stage/usr" | cut -f1)"
sed -e "s/@ARCH@/$arch/" -e "s/@VERSION@/$version/" "$root/packaging/control.in" > "$stage/DEBIAN/control"
printf 'Installed-Size: %s\n' "$size_kib" >> "$stage/DEBIAN/control"
install -m 0755 "$root/packaging/postinst" "$stage/DEBIAN/postinst"

echo "==> build .deb"
mkdir -p "$root/artifacts"
# --root-owner-group (dpkg >= 1.19): root:root files without fakeroot.
dpkg-deb --build --root-owner-group "$stage" "$out"

echo "==> built $out"
dpkg-deb --info "$out"
echo "--- contents ---"
dpkg-deb --contents "$out"
