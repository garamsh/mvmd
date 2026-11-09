#!/bin/bash
# Builds a statically linked mkfs.erofs, and the fsck.erofs the fixtures read its output with, from
# pinned source tarballs (ADR 0017). Every input below is verified by SHA-256 before it is unpacked;
# a version moves by editing its url and digest here, in a pull request that reruns the fixtures.
# The host supplies gcc, make, perl, curl, tar, xz, gzip, sha256sum, binutils and pkg-config; the rest
# of the toolchain is built here, so no header or library of the host enters the result.
#
# Usage: build.sh <cache-dir> <output-dir> [--rebuild]
# <cache-dir> holds downloads, the toolchain and the two builds; <output-dir> receives
# mkfs.erofs and fsck.erofs, which are built twice in different directories and must be identical.
# Where <output-dir> already holds tools built by this very script, nothing is done unless
# --rebuild is given.
set -euo pipefail

M4=https://ftp.gnu.org/gnu/m4/m4-1.4.21.tar.xz
M4_SHA256=f25c6ab51548a73a75558742fb031e0625d6485fe5f9155949d6486a2408ab66
AUTOCONF=https://ftp.gnu.org/gnu/autoconf/autoconf-2.73.tar.xz
AUTOCONF_SHA256=9fd672b1c8425fac2fa67fa0477b990987268b90ff36d5f016dae57be0d6b52e
AUTOMAKE=https://ftp.gnu.org/gnu/automake/automake-1.20.tar.xz
AUTOMAKE_SHA256=f6f34dad4cbe395715762b0710f7c511e39bd5cf777e6b95818ccbecf7714248
LIBTOOL=https://ftp.gnu.org/gnu/libtool/libtool-2.6.2.tar.xz
LIBTOOL_SHA256=2ef1067c16c97db930fd740cc9bc3d3ba9a583804ae5ac42cc3e8719e49e191e
MUSL=https://musl.libc.org/releases/musl-1.2.6.tar.gz
MUSL_SHA256=d585fd3b613c66151fc3249e8ed44f77020cb5e6c1e635a616d3f9f82460512a
KERNEL_HEADERS=https://github.com/sabotage-linux/kernel-headers/archive/refs/tags/v6.12.77.tar.gz
KERNEL_HEADERS_SHA256=cd70720ccdae889ffb4c4e7764f11b8c26add20d6bde78c0012225d52d38d3cb
ZLIB=https://github.com/madler/zlib/releases/download/v1.4/zlib-1.4.tar.xz
ZLIB_SHA256=4f11c512053d7bab8d9e9b89bde3575add3e4c50a383b42ef6cb837035fc5a7c
# erofs-utils v1.9.4, named by the commit the tag points to, so the URL cannot come to mean another tree.
EROFS=https://github.com/erofs/erofs-utils/archive/f36cadb5c563995ab3aa8572a60ed6b721b9557d.tar.gz
EROFS_SHA256=8cd7818c2733f95e91cc830bbb08648054b88181e7cfe130d60d346824a70c94
EROFS_VERSION=1.9.4

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ] || { [ "$#" -eq 3 ] && [ "$3" != --rebuild ]; }; then
  echo "usage: $0 <cache-dir> <output-dir> [--rebuild]" >&2
  exit 2
fi
mkdir -p "$1" "$2"
cache=$(cd "$1" && pwd)
out=$(cd "$2" && pwd)

# The tools in <output-dir> are current when they were built by this script as it is now: its
# digest holds every pin.
script_digest=$(sha256sum "$0" | cut -d' ' -f1)
if [ "${3:-}" != --rebuild ] && [ -x "$out/mkfs.erofs" ] && [ -x "$out/fsck.erofs" ] \
  && [ "$(cat "$out/built-by" 2>/dev/null)" = "$script_digest" ]; then
  echo "build.sh: $out holds the tools this script built, $("$out/mkfs.erofs" --version 2>&1 | head -n1)"
  exit 0
fi
prefix=$cache/prefix
jobs=$(nproc)

export LC_ALL=C TZ=UTC SOURCE_DATE_EPOCH=0
unset CFLAGS CPPFLAGS LDFLAGS LIBS CC PKG_CONFIG_PATH
export PATH="$prefix/bin:$PATH"

case "$(uname -m)" in
  x86_64) kernel_arch=x86_64 ;;
  aarch64) kernel_arch=arm64 ;;
  *) echo "build.sh: no kernel header architecture for $(uname -m)" >&2; exit 1 ;;
esac

# fetch <url> <sha256> prints the path of the verified download.
fetch() {
  local file="$cache/dl/${1##*/}"
  mkdir -p "$cache/dl"
  if [ ! -f "$file" ]; then
    curl --fail --silent --show-error --location --output "$file.part" "$1"
    mv "$file.part" "$file"
  fi
  echo "$2  $file" | sha256sum --check --quiet >&2 || {
    echo "build.sh: $file does not match its pinned SHA-256 $2" >&2
    rm -f "$file"
    exit 1
  }
  echo "$file"
}

# unpack <tarball> <dir> extracts into a fresh <dir>, dropping the tarball's top directory.
unpack() {
  rm -rf "$2"
  mkdir -p "$2"
  tar -xf "$1" -C "$2" --strip-components=1
}

# stage <name> <command...> runs a command once; a stamp records that it finished.
stage() {
  local name=$1
  shift
  if [ -f "$prefix/.stamp-$name" ]; then
    return
  fi
  echo "build.sh: building $name" >&2
  "$@"
  touch "$prefix/.stamp-$name"
}

gnu_tool() {
  local src="$cache/src/$1"
  unpack "$(fetch "$2" "$3")" "$src"
  (cd "$src" && ./configure --prefix="$prefix" >/dev/null && make -j"$jobs" >/dev/null && make install >/dev/null)
}

build_musl() {
  local src="$cache/src/musl"
  unpack "$(fetch "$MUSL" "$MUSL_SHA256")" "$src"
  (cd "$src" && ./configure --prefix="$prefix/musl" --disable-shared >/dev/null && make -j"$jobs" >/dev/null && make install >/dev/null)
}

build_kernel_headers() {
  local src="$cache/src/kernel-headers"
  unpack "$(fetch "$KERNEL_HEADERS" "$KERNEL_HEADERS_SHA256")" "$src"
  (cd "$src" && make ARCH="$kernel_arch" prefix="$prefix/musl" install >/dev/null)
}

build_zlib() {
  local src="$cache/src/zlib"
  unpack "$(fetch "$ZLIB" "$ZLIB_SHA256")" "$src"
  (cd "$src" && CC="$prefix/musl/bin/musl-gcc" ./configure --prefix="$prefix/musl" --static >/dev/null \
    && make -j"$jobs" libz.a >/dev/null && make install >/dev/null)
}

prepare_erofs() {
  local src="$cache/src/erofs-utils"
  unpack "$(fetch "$EROFS" "$EROFS_SHA256")" "$src"
  if [ ! -f /usr/share/aclocal/pkg.m4 ]; then
    echo "build.sh: pkg.m4 is not in /usr/share/aclocal; install pkg-config" >&2
    exit 1
  fi
  # Only pkg.m4 is taken from the host: its other macros, libtool's among them, would be older
  # than the tools built above.
  mkdir -p "$cache/aclocal"
  cp /usr/share/aclocal/pkg.m4 "$cache/aclocal/"
  (cd "$src" && ACLOCAL_PATH="$cache/aclocal" sh autogen.sh >/dev/null 2>&1)
}

# build_erofs <dir> <result-dir> configures and builds out of the tree in <dir>, with every path
# the build sees remapped to a fixed name.
build_erofs() {
  local src="$cache/src/erofs-utils" dir=$1 result=$2
  rm -rf "$dir"
  mkdir -p "$dir" "$result"
  (
    cd "$dir"
    PKG_CONFIG_LIBDIR="$prefix/musl/lib/pkgconfig" \
    CC="$prefix/musl/bin/musl-gcc" \
    CFLAGS="-O2 -fno-pie -ffile-prefix-map=$src=/src -ffile-prefix-map=$dir=/build" \
    LDFLAGS="-static -no-pie -Wl,--build-id=none" \
      "$src/configure" --disable-shared --enable-static --without-uuid --disable-lz4 --disable-lzma \
        --without-libdeflate --without-libzstd --without-qpl --without-xxhash --without-libcurl \
        --without-openssl --without-libxml2 --without-json-c --without-libnl3 --disable-fuse \
        --disable-s3 --disable-oci --disable-fanotify --disable-ublk --without-selinux >/dev/null
    make -j"$jobs" -C lib >/dev/null
    make -j"$jobs" -C mkfs >/dev/null
    make -j"$jobs" -C fsck >/dev/null
    for bin in mkfs/mkfs.erofs fsck/fsck.erofs; do
      strip --strip-all -o "$result/$(basename "$bin")" "$bin"
    done
  )
}

stage m4 gnu_tool m4 "$M4" "$M4_SHA256"
stage autoconf gnu_tool autoconf "$AUTOCONF" "$AUTOCONF_SHA256"
stage automake gnu_tool automake "$AUTOMAKE" "$AUTOMAKE_SHA256"
stage libtool gnu_tool libtool "$LIBTOOL" "$LIBTOOL_SHA256"
stage musl build_musl
stage kernel-headers build_kernel_headers
stage zlib build_zlib
stage erofs-utils-source prepare_erofs

second=$(mktemp -d)
trap 'rm -rf "$second"' EXIT
build_erofs "$cache/build" "$cache/result"
build_erofs "$second/build" "$second/result"
for bin in mkfs.erofs fsck.erofs; do
  if ! cmp -s "$cache/result/$bin" "$second/result/$bin"; then
    echo "build.sh: the two $bin builds differ" >&2
    exit 1
  fi
done

# A static executable has no program interpreter and no dynamic section.
if readelf -l "$cache/result/mkfs.erofs" | grep -qE 'INTERP|DYNAMIC'; then
  echo "build.sh: mkfs.erofs is dynamically linked" >&2
  exit 1
fi
version=$("$cache/result/mkfs.erofs" --version 2>&1 | head -n1)
case "$version" in
  *"$EROFS_VERSION"*) ;;
  *) echo "build.sh: mkfs.erofs reports '$version', expected $EROFS_VERSION" >&2; exit 1 ;;
esac

install -m 0755 "$cache/result/mkfs.erofs" "$out/mkfs.erofs"
install -m 0755 "$cache/result/fsck.erofs" "$out/fsck.erofs"
echo "$script_digest" >"$out/built-by"
echo "build.sh: $version, statically linked, both builds give $(sha256sum "$out/mkfs.erofs" | cut -d' ' -f1)"
