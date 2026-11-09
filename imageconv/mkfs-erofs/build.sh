#!/bin/bash
# Builds a statically linked mkfs.erofs, and the fsck.erofs the fixtures read its output with, from
# pinned inputs (ADR 0017). Every input below is verified by SHA-256 before it is unpacked; a
# version moves by editing its url and digest here, in a pull request that reruns the fixtures.
# The compiler is a pinned zig, which carries clang, lld, musl and the Linux headers, so the result
# depends on no compiler, linker, header or library of the host. The host supplies a shell, make,
# perl, curl, tar, xz, gzip, sha256sum and readelf (a check on the result, not an input to it).
#
# Usage: build.sh <cache-dir> <output-dir> [--rebuild]
# <cache-dir> holds downloads, the toolchain and the two builds; <output-dir> receives
# mkfs.erofs and fsck.erofs, which are built twice in different directories and must be identical.
# Where <output-dir> already holds tools built by this very script, nothing is done unless
# --rebuild is given.
set -euo pipefail

# zig 0.17.1: the digests are those ziglang.org publishes in download/index.json.
ZIG_X86_64=https://ziglang.org/download/0.17.1/zig-x86_64-linux-0.17.1.tar.xz
ZIG_X86_64_SHA256=3581863e3cbcfd0fce0efc682ce7e9e93da96668a977791d356a26d2a7e9b17a
ZIG_AARCH64=https://ziglang.org/download/0.17.1/zig-aarch64-linux-0.17.1.tar.xz
ZIG_AARCH64_SHA256=a1546d5646eb184437918d6635ca6b762a0e889eac09cef720ee7a1ed3fe7795
M4_URL=https://ftp.gnu.org/gnu/m4/m4-1.4.21.tar.xz
M4_SHA256=f25c6ab51548a73a75558742fb031e0625d6485fe5f9155949d6486a2408ab66
AUTOCONF_URL=https://ftp.gnu.org/gnu/autoconf/autoconf-2.73.tar.xz
AUTOCONF_SHA256=9fd672b1c8425fac2fa67fa0477b990987268b90ff36d5f016dae57be0d6b52e
AUTOMAKE_URL=https://ftp.gnu.org/gnu/automake/automake-1.20.tar.xz
AUTOMAKE_SHA256=f6f34dad4cbe395715762b0710f7c511e39bd5cf777e6b95818ccbecf7714248
LIBTOOL_URL=https://ftp.gnu.org/gnu/libtool/libtool-2.6.2.tar.xz
LIBTOOL_SHA256=2ef1067c16c97db930fd740cc9bc3d3ba9a583804ae5ac42cc3e8719e49e191e
# Only its pkg.m4 is used: the macros erofs-utils's configure.ac expands for its libraries.
PKGCONF=https://distfiles.ariadne.space/pkgconf/pkgconf-3.0.10.tar.xz
PKGCONF_SHA256=9371b2f6372da574d4c6dc96fe9b1463037bcf6afcb6671c1555afde7b17e2de
ZLIB=https://github.com/madler/zlib/releases/download/v1.4/zlib-1.4.tar.xz
# zlib.net serves the same file; its content was checked against the digest below on 2026-11-09.
ZLIB_MIRROR=https://zlib.net/zlib-1.4.tar.xz
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

case "$(uname -m)" in
  x86_64) zig_url=$ZIG_X86_64 zig_sha256=$ZIG_X86_64_SHA256 triple=x86_64-linux-musl ;;
  aarch64) zig_url=$ZIG_AARCH64 zig_sha256=$ZIG_AARCH64_SHA256 triple=aarch64-linux-musl ;;
  *) echo "build.sh: no zig is pinned for $(uname -m)" >&2; exit 1 ;;
esac

# Nothing of the host's build environment may reach a tool: each variable below changes what an
# autotools program reads or which compiler it finds.
unset CC CXX CPP CFLAGS CPPFLAGS CXXFLAGS LDFLAGS LIBS AR RANLIB PKG_CONFIG PKG_CONFIG_PATH \
  PKG_CONFIG_LIBDIR ACLOCAL ACLOCAL_PATH ACLOCAL_AMFLAGS AUTOCONF AUTOHEADER AUTOMAKE AUTOM4TE \
  AUTOCONF_ARGS LIBTOOLIZE LIBTOOL M4 M4PATH PERL5LIB PERLLIB TMPDIR
export LC_ALL=C TZ=UTC SOURCE_DATE_EPOCH=0
export PATH="$prefix/bin:$PATH"
export ZIG_GLOBAL_CACHE_DIR="$cache/zig-cache" ZIG_LOCAL_CACHE_DIR="$cache/zig-cache/local"

# fetch <sha256> <url>... takes the file the pinned digest names from the cache, or else from the
# first of the URLs, tried in order, that gives a file with that digest. The digest decides, not
# the host: a URL that fails, or answers with other content, is reported and the next is tried.
# The file is checked against the digest whether it was just downloaded or came from the cache.
# All URLs name one file, so the cache path is the last component of the first. $fetched is set
# to the path. fetch is called as a command of its own and never inside a command substitution,
# whose subshell would swallow its exit and let the build carry on with an empty path.
#
# A download that fails is reported with its URL, curl's own error on the line above, and the
# build stops once every URL has failed. Each URL is tried with at most 4 retries after the first
# attempt, curl waiting 1, 2, 4 and 8 seconds between them (its default doubling), so at most 5
# attempts and 15 seconds of waiting per URL; --retry-all-errors includes an HTTP 5xx under
# --fail.
fetched=
fetch() {
  local sha256=$1
  shift
  local file="$cache/dl/${1##*/}" url status
  mkdir -p "$cache/dl"
  if [ -f "$file" ]; then
    if ! echo "$sha256  $file" | sha256sum --check --quiet >&2; then
      echo "build.sh: $file does not match its pinned SHA-256 $sha256" >&2
      rm -f "$file"
      exit 1
    fi
    fetched=$file
    return
  fi
  for url in "$@"; do
    status=0
    curl --fail --silent --show-error --location --connect-timeout 30 \
      --retry 4 --retry-all-errors --output "$file.part" "$url" || status=$?
    if [ "$status" -ne 0 ]; then
      rm -f "$file.part"
      echo "build.sh: downloading $url failed (curl exit status $status)" >&2
    elif echo "$sha256  $file.part" | sha256sum --check --quiet >&2; then
      mv "$file.part" "$file"
      fetched=$file
      return
    else
      rm -f "$file.part"
      echo "build.sh: $url does not match its pinned SHA-256 $sha256" >&2
    fi
  done
  echo "build.sh: no URL for ${file##*/} gave a file with the pinned SHA-256 $sha256" >&2
  exit 1
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

# The compiler wrappers: `cc` builds for musl, `hostcc` for the machine the build runs on.
install_zig() {
  fetch "$zig_sha256" "$zig_url"
  unpack "$fetched" "$cache/zig"
  mkdir -p "$prefix/bin" "$cache/wrap"
  local zig="$cache/zig/zig"
  printf '#!/bin/sh\nexec "%s" cc -target %s "$@"\n' "$zig" "$triple" >"$cache/wrap/cc"
  printf '#!/bin/sh\nexec "%s" cc "$@"\n' "$zig" >"$cache/wrap/hostcc"
  printf '#!/bin/sh\nexec "%s" ar "$@"\n' "$zig" >"$cache/wrap/ar"
  printf '#!/bin/sh\nexec "%s" ranlib "$@"\n' "$zig" >"$cache/wrap/ranlib"
  chmod +x "$cache/wrap/cc" "$cache/wrap/hostcc" "$cache/wrap/ar" "$cache/wrap/ranlib"
}

gnu_tool() {
  local src="$cache/src/$1"
  # ftpmirror.gnu.org redirects to a nearby mirror of ftp.gnu.org's files.
  fetch "$3" "$2" "https://ftpmirror.gnu.org/gnu/${2#https://ftp.gnu.org/gnu/}"
  unpack "$fetched" "$src"
  (cd "$src" && CC="$cache/wrap/hostcc" ./configure --prefix="$prefix" >/dev/null \
    && make -j"$jobs" >/dev/null && make install >/dev/null)
}

build_zlib() {
  local src="$cache/src/zlib"
  fetch "$ZLIB_SHA256" "$ZLIB" "$ZLIB_MIRROR"
  unpack "$fetched" "$src"
  (cd "$src" && CC="$cache/wrap/cc" AR="$cache/wrap/ar" RANLIB="$cache/wrap/ranlib" \
    ./configure --prefix="$prefix/zlib" --static >/dev/null \
    && make -j"$jobs" libz.a >/dev/null && make install >/dev/null)
}

# The macros aclocal reads are those of the tools built above, in $prefix/share/aclocal, and the
# one file below. No directory of the host is searched.
prepare_erofs() {
  local src="$cache/src/erofs-utils" pkgconf="$cache/src/pkgconf"
  fetch "$EROFS_SHA256" "$EROFS"
  unpack "$fetched" "$src"
  fetch "$PKGCONF_SHA256" "$PKGCONF"
  unpack "$fetched" "$pkgconf"
  rm -rf "$cache/aclocal"
  mkdir -p "$cache/aclocal"
  cp "$pkgconf/pkg.m4" "$cache/aclocal/"
  local acdir
  acdir=$(aclocal --print-ac-dir)
  if [ "$acdir" != "$prefix/share/aclocal" ]; then
    echo "build.sh: aclocal reads $acdir, not $prefix/share/aclocal" >&2
    exit 1
  fi
  (cd "$src" && ACLOCAL_PATH="$cache/aclocal" sh autogen.sh >/dev/null 2>&1)
  # The generated configure carries the version of the libtool macros it was made from.
  local libtool_version=${LIBTOOL_URL##*/libtool-}
  libtool_version=${libtool_version%.tar.xz}
  if ! grep -q "^macro_version='$libtool_version'" "$src/configure"; then
    echo "build.sh: configure holds libtool macros that are not libtool $libtool_version's" >&2
    exit 1
  fi
}

# build_erofs <dir> <result-dir> configures and builds out of the tree in <dir>, with every path
# the build sees remapped to a fixed name. The libraries are named to configure directly, so
# pkg-config is never run.
build_erofs() {
  local src="$cache/src/erofs-utils" dir=$1 result=$2
  rm -rf "$dir"
  mkdir -p "$dir" "$result"
  (
    cd "$dir"
    PKG_CONFIG=false \
    zlib_CFLAGS="-I$prefix/zlib/include" zlib_LIBS="-L$prefix/zlib/lib -lz" \
    CC="$cache/wrap/cc" AR="$cache/wrap/ar" RANLIB="$cache/wrap/ranlib" \
    CPPFLAGS="-I$prefix/zlib/include" \
    CFLAGS="-O2 -fno-pie -ffile-prefix-map=$src=/src -ffile-prefix-map=$dir=/build" \
    LDFLAGS="-static -no-pie -s -L$prefix/zlib/lib -Wl,--build-id=none" \
      "$src/configure" --disable-shared --enable-static --without-uuid --disable-lz4 --disable-lzma \
        --without-libdeflate --without-libzstd --without-qpl --without-xxhash --without-libcurl \
        --without-openssl --without-libxml2 --without-json-c --without-libnl3 --disable-fuse \
        --disable-s3 --disable-oci --disable-fanotify --disable-ublk --without-selinux >/dev/null
    make -j"$jobs" -C lib >/dev/null
    make -j"$jobs" -C mkfs >/dev/null
    make -j"$jobs" -C fsck >/dev/null
    cp mkfs/mkfs.erofs fsck/fsck.erofs "$result/"
  )
}

stage zig install_zig
stage m4 gnu_tool m4 "$M4_URL" "$M4_SHA256"
stage autoconf gnu_tool autoconf "$AUTOCONF_URL" "$AUTOCONF_SHA256"
stage automake gnu_tool automake "$AUTOMAKE_URL" "$AUTOMAKE_SHA256"
stage libtool gnu_tool libtool "$LIBTOOL_URL" "$LIBTOOL_SHA256"
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
