#!/bin/bash
# Fetches the pinned Firecracker binary and the guest kernel the Driver's tests boot, each from its
# release and each verified by SHA-256 before it is unpacked or used (ADR 0010, driver.md). A
# version moves by editing its url and digests here, in a pull request that reruns
# `make test-firecracker`; the digests below are the ones its release publishes, checked against
# the files as downloaded. The host supplies bash, curl, tar, zstd and sha256sum.
#
# Moved by: the PM, who reviews these pins monthly against both projects' releases and security
# advisories, and at once when an advisory names a pinned version (driver.md). No bot proposes it.
#
# Usage: fetch.sh <output-dir>
# <output-dir> keeps the downloads in dl/ and receives firecracker, vmlinux and inputs.env, which names both with the digests
# pinned here and not the ones read back from the files, so a test that checks a file against
# them checks it against the pin. Files already there that match their pins are not fetched again.
set -euo pipefail

# Firecracker v1.17.0. The tarball digest is the one the GitHub release lists for the asset; the
# binary's is the line of the release's own SHA256SUMS.
FIRECRACKER_VERSION=v1.17.0
FIRECRACKER_URL_BASE=https://github.com/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION
FIRECRACKER_TGZ_SHA256_x86_64=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
FIRECRACKER_BIN_SHA256_x86_64=99ad0f5cd0514a88aad0e9ae8cfdb3cc3b4ab9d190e1194602406c786b5de7a5
FIRECRACKER_TGZ_SHA256_aarch64=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
FIRECRACKER_BIN_SHA256_aarch64=fe726e0b43c04363ac07e358be4dee982c3947c65ed3ae10c770fef5e1cd756c

# Kata Containers 4.3.1, kernel 6.18.56 (guest.md, "Kernel candidates checked"). The tarball
# digests are the ones the GitHub release lists; the kernels' are the SHA-256 of
# opt/kata/share/kata-containers/vmlinux-6.18.56-208 taken from each tarball.
KATA_URL_BASE=https://github.com/kata-containers/kata-containers/releases/download/4.3.1
KATA_TAR_SHA256_x86_64=1463a2b86845f4d3d8e87f2f764ec59856fc0fee3520967a79ade32762be2e97
KATA_KERNEL_SHA256_x86_64=f87d8359035cb7557b16a319e6431ea09b183e62268aef436bf8dc6530d786b6
KATA_TAR_SHA256_aarch64=2c5bf7cde334ad5307d482f45cceda496f91c9b6b96f5278c47f4f4eac1bbae6
KATA_KERNEL_SHA256_aarch64=dece388830720fe8bddb41552fff5f486240aeece5acc0aebc1812761e3e3f66
KATA_KERNEL_PATH=./opt/kata/share/kata-containers/vmlinux-6.18.56-208

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <output-dir>" >&2
  exit 2
fi
mkdir -p "$1"
out=$(cd "$1" && pwd)

case "$(uname -m)" in
  x86_64) arch=x86_64 kata_arch=amd64 ;;
  aarch64 | arm64) arch=aarch64 kata_arch=arm64 ;;
  *)
    echo "fetch.sh: no pins for $(uname -m)" >&2
    exit 1
    ;;
esac
pin() { local name=$1_$arch; echo "${!name}"; }

# digest_is succeeds when the file exists and has the SHA-256.
digest_is() { [ -f "$1" ] && [ "$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ]; }

# verified fails, naming the file, unless it has the SHA-256.
verified() {
  local got
  got=$(sha256sum "$1" | cut -d' ' -f1)
  if [ "$got" != "$2" ]; then
    echo "fetch.sh: $1 has SHA-256 $got, not the pinned $2" >&2
    exit 1
  fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# download <url> <sha256> puts the file the digest names at $downloaded: the copy kept in
# <output-dir>/dl when its digest matches, else a fresh download kept there once its digest
# matches. The digest is checked on every use, cache hits included. A download that fails is retried
# by curl, which waits 1, 2, 4 and 8 seconds between at most 5 attempts (15 seconds of waiting),
# --retry-all-errors including an HTTP 5xx, which GitHub's release host returns now and then; each
# attempt gives up connecting after 30 seconds. Neither project publishes a second official URL for
# these files, so there is none to fall back to; the pipeline keeps <output-dir>/dl between runs.
download() {
  local file="$out/dl/${1##*/}"
  mkdir -p "$out/dl"
  if ! digest_is "$file" "$2"; then
    rm -f "$file" "$file.part"
    curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
      --output "$file.part" "$1" || { echo "fetch.sh: downloading $1 failed" >&2; exit 1; }
    verified "$file.part" "$2"
    mv "$file.part" "$file"
  fi
  downloaded=$file
}

fc_bin_sha256=$(pin FIRECRACKER_BIN_SHA256)
if digest_is "$out/firecracker" "$fc_bin_sha256"; then
  echo "fetch.sh: $out/firecracker is the pinned Firecracker $FIRECRACKER_VERSION"
else
  tgz=firecracker-$FIRECRACKER_VERSION-$arch.tgz
  download "$FIRECRACKER_URL_BASE/$tgz" "$(pin FIRECRACKER_TGZ_SHA256)"
  tar -xzf "$downloaded" -C "$work" "release-$FIRECRACKER_VERSION-$arch/firecracker-$FIRECRACKER_VERSION-$arch"
  verified "$work/release-$FIRECRACKER_VERSION-$arch/firecracker-$FIRECRACKER_VERSION-$arch" "$fc_bin_sha256"
  install -m 0755 "$work/release-$FIRECRACKER_VERSION-$arch/firecracker-$FIRECRACKER_VERSION-$arch" "$out/firecracker"
  echo "fetch.sh: fetched Firecracker $FIRECRACKER_VERSION, SHA-256 $fc_bin_sha256"
fi

kernel_sha256=$(pin KATA_KERNEL_SHA256)
if digest_is "$out/vmlinux" "$kernel_sha256"; then
  echo "fetch.sh: $out/vmlinux is the pinned kernel"
else
  tar_name=kata-static-4.3.1-$kata_arch.tar.zst
  download "$KATA_URL_BASE/$tar_name" "$(pin KATA_TAR_SHA256)"
  mkdir "$work/kata"
  zstd --decompress --stdout "$downloaded" | tar -x -C "$work/kata" "$KATA_KERNEL_PATH"
  verified "$work/kata/$KATA_KERNEL_PATH" "$kernel_sha256"
  install -m 0644 "$work/kata/$KATA_KERNEL_PATH" "$out/vmlinux"
  echo "fetch.sh: fetched the Kata Containers 4.3.1 kernel, SHA-256 $kernel_sha256"
fi

cat >"$out/inputs.env" <<EOF
MVMD_FIRECRACKER=$out/firecracker
MVMD_FIRECRACKER_SHA256=$fc_bin_sha256
MVMD_KERNEL=$out/vmlinux
MVMD_KERNEL_SHA256=$kernel_sha256
EOF
