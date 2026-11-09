#!/bin/bash
# Fetches the pinned Cloud Hypervisor binary the Driver's tests boot, from its release and verified by
# SHA-256 before it is used (ADR 0010, driver.md). The guest kernel is the one
# tooling/firecracker/fetch.sh pins, and `make test-cloud-hypervisor` runs that script for it, so
# the kernel is pinned in one place. A version moves by editing its url and digests here, in a pull
# request that reruns `make test-cloud-hypervisor`; the digests below are the ones the GitHub
# release lists for each asset (it publishes no SHA256SUMS file), checked against the files as
# downloaded. The host supplies bash, curl and sha256sum.
#
# Moved by: the PM, who reviews these pins monthly against the project's releases and security
# advisories, and at once when an advisory names the pinned version (driver.md). No bot proposes it.
#
# Usage: fetch.sh <output-dir>
# <output-dir> keeps the download in dl/ and receives cloud-hypervisor and inputs.env, which names
# the binary with the digest pinned here and not the one read back from the file, so a test that
# checks the file against it checks it against the pin. A file already there that matches its pin is
# not fetched again.
set -euo pipefail

# Cloud Hypervisor v54.0, the statically linked build.
CLOUD_HYPERVISOR_VERSION=v54.0
CLOUD_HYPERVISOR_URL_BASE=https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CLOUD_HYPERVISOR_VERSION
CLOUD_HYPERVISOR_SHA256_x86_64=4f4f98a3d661a40f3e5272627cc895a1b2e525f2f6bedf5f895f9cd21ba5c65b
CLOUD_HYPERVISOR_SHA256_aarch64=e8a6d9786ea678e739f1a386682846e93742dcb9c7b6e3e0c62e3bda6cff0ef3
CLOUD_HYPERVISOR_ASSET_x86_64=cloud-hypervisor-static
CLOUD_HYPERVISOR_ASSET_aarch64=cloud-hypervisor-static-aarch64

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <output-dir>" >&2
  exit 2
fi
mkdir -p "$1"
out=$(cd "$1" && pwd)

case "$(uname -m)" in
  x86_64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
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

bin_sha256=$(pin CLOUD_HYPERVISOR_SHA256)
if digest_is "$out/cloud-hypervisor" "$bin_sha256"; then
  echo "fetch.sh: $out/cloud-hypervisor is the pinned Cloud Hypervisor $CLOUD_HYPERVISOR_VERSION"
else
  # The download is kept in <output-dir>/dl, which the pipeline keeps between runs. curl retries
  # at most 5 attempts, waiting 1, 2, 4 and 8 seconds between them, --retry-all-errors including an
  # HTTP 5xx, which GitHub's release host returns now and then; each attempt gives up connecting
  # after 30 seconds. The project publishes no second official URL, so there is none to fall back to.
  asset=$(pin CLOUD_HYPERVISOR_ASSET)
  mkdir -p "$out/dl"
  file="$out/dl/$CLOUD_HYPERVISOR_VERSION-$asset"
  if ! digest_is "$file" "$bin_sha256"; then
    rm -f "$file" "$file.part"
    curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
      --output "$file.part" "$CLOUD_HYPERVISOR_URL_BASE/$asset" || { echo "fetch.sh: downloading $asset failed" >&2; exit 1; }
    verified "$file.part" "$bin_sha256"
    mv "$file.part" "$file"
  fi
  verified "$file" "$bin_sha256"
  install -m 0755 "$file" "$out/cloud-hypervisor"
  echo "fetch.sh: fetched Cloud Hypervisor $CLOUD_HYPERVISOR_VERSION, SHA-256 $bin_sha256"
fi

cat >"$out/inputs.env" <<EOT
MVMD_CLOUD_HYPERVISOR=$out/cloud-hypervisor
MVMD_CLOUD_HYPERVISOR_SHA256=$bin_sha256
EOT
