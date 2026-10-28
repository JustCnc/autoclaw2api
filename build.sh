#!/usr/bin/env bash
# Build the plugin as a C shared library.
#
#   ./build.sh                    # windows/amd64 + linux/amd64 (when toolchains exist)
#   ./build.sh windows/amd64
#   ./build.sh linux/amd64
#
# Windows needs x86_64-w64-mingw32-gcc (MSYS2 / MinGW). Linux needs a Linux C
# compiler (native gcc, x86_64-linux-gnu-gcc, or `zig cc`).
set -euo pipefail
cd "$(dirname "$0")"

NAME=autoclaw2api
GO=${GO:-go}
OUT_DIR=${OUT_DIR:-$(pwd)/dist/plugins}
TARGETS=${*:-"windows/amd64 linux/amd64"}

if ! command -v "$GO" >/dev/null 2>&1; then
  echo "go not found; set GO=/path/to/go" >&2
  exit 2
fi

mkdir -p "$OUT_DIR"

native_cc() {
  if command -v "${CC_LINUX:-}" >/dev/null 2>&1; then echo "${CC_LINUX}"; return; fi
  if command -v x86_64-linux-gnu-gcc >/dev/null 2>&1; then echo x86_64-linux-gnu-gcc; return; fi
  if command -v gcc >/dev/null 2>&1; then echo gcc; return; fi
  echo ""
}

build_target() {
  local target="$1"
  local goos="${target%%/*}"
  local goarch="${target##*/}"
  local ext cc
  case "$goos" in
    windows)
      ext=dll
      cc=${CC_WINDOWS:-x86_64-w64-mingw32-gcc}
      ;;
    linux)
      ext=so
      if [[ "$goarch" == "arm64" ]]; then
        cc=${CC_LINUX_ARM64:-aarch64-linux-gnu-gcc}
      else
        cc=$(native_cc)
      fi
      ;;
    *)
      echo "unsupported target: $target" >&2
      return 1
      ;;
  esac
  if ! command -v "${cc%% *}" >/dev/null 2>&1; then
    echo "skip $target: C compiler '$cc' not found" >&2
    return 0
  fi
  local out="$OUT_DIR/$goos/$goarch"
  mkdir -p "$out"
  echo "==> $target (CC=$cc)"
  CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="$cc" \
    "$GO" build -buildmode=c-shared -trimpath -ldflags="-s -w" \
    -o "$out/$NAME.$ext" .
  rm -f "$out/$NAME.h"
  echo "    $out/$NAME.$ext"
}

for target in $TARGETS; do
  build_target "$target"
done

echo
echo "artifacts:"
find "$OUT_DIR" -type f \( -name "*.so" -o -name "*.dll" -o -name "*.dylib" \) -printf '  %p (%s bytes)\n'
