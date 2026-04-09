#!/usr/bin/env bash
# Release build script for speedtest (Go).
#
# Practices (aligned with common production guidance):
#   - CGO_ENABLED=0     Static binary, no libc/cgo at runtime (portable Linux, smaller attack surface).
#   -trimpath            Omit host filesystem paths from the binary (reproducibility, less path leakage).
#   -ldflags -s -w       Omit symbol table and DWARF (smaller binary; no debug metadata in the executable).
#   -tags netgo,osusergo Prefer pure-Go net/user resolution when applicable (typical with CGO off on Linux).
#   -ldflags -X          Inject git-derived version into main.buildVersion (optional override via VERSION=).
#
# Optional environment (see `go help environment`):
#   VERSION=1.2.3       Override embedded version string (default: git describe --tags --always --dirty).
#   GOAMD64=v2          Use AMD64 v2 ISA for amd64 targets (SSE4.2); unset = default v1 (broadest CPUs).
#   GOARM64=v8.0        ARM64 baseline; can use v8.2, v8.5, etc. on supported toolchains.
#
# Usage:
#   ./build.sh              # build all default artifacts
#   ./build.sh clean        # remove built binaries in this directory
#   ./build.sh help         # show this message

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

MAIN="main.go"
# Tags: netgo/osusergo are widely recommended for static Linux releases (pure Go resolver / user db).
GOTAGS="${GOTAGS:-netgo,osusergo}"

embed_version() {
	if [[ -n "${VERSION:-}" ]]; then
		printf '%s' "$VERSION"
		return
	fi
	local d
	d=$(git describe --tags --always --dirty 2>/dev/null) || true
	if [[ -n "$d" ]]; then
		printf '%s' "$d"
	else
		printf 'unknown'
	fi
}

ldflags_release() {
	local v
	v="$(embed_version)"
	# -X sets main.buildVersion at link time (see var buildVersion in main.go).
	printf -- '-s -w -X=main.buildVersion=%s' "$v"
}

build_one() {
	local out=$1
	CGO_ENABLED=0 go build \
		-trimpath \
		-tags="${GOTAGS}" \
		-ldflags "$(ldflags_release)" \
		-o "$out" \
		"$MAIN"
	echo "built: $out"
}

build_native() {
	build_one speedtest
}

build_linux_amd64() {
	GOOS=linux GOARCH=amd64 build_one speedtest-linux-amd64
}

build_linux_arm64() {
	GOOS=linux GOARCH=arm64 build_one speedtest-linux-arm64
}

build_all() {
	echo "Go: $(go version)"
	go mod verify
	build_native
	build_linux_amd64
	build_linux_arm64
}

do_clean() {
	rm -f speedtest speedtest-linux-amd64 speedtest-linux-arm64
	echo "removed local release binaries"
}

usage() {
	cat <<'EOF'
Usage: ./build.sh [command]

Commands:
  all (default)   Build speedtest, speedtest-linux-amd64, speedtest-linux-arm64
  native          Build only the binary for the current OS/arch (speedtest)
  linux-amd64     Cross-build Linux amd64
  linux-arm64     Cross-build Linux arm64
  clean           Remove built binaries in this directory
  help            Show this message

Environment:
  VERSION         Embedded version (default: git describe --tags --always --dirty)
  GOTAGS          Go build tags (default: netgo,osusergo)
  GOAMD64         e.g. v2 for amd64 targets (optional)
  GOARM64         ARM64 baseline variant (optional)

See comment header in this script for production build notes.
EOF
}

case "${1:-all}" in
all)
	build_all
	;;
clean)
	do_clean
	;;
help|-h|--help)
	usage
	;;
native)
	build_native
	;;
linux-amd64)
	build_linux_amd64
	;;
linux-arm64)
	build_linux_arm64
	;;
*)
	echo "unknown command: $1" >&2
	usage >&2
	exit 1
	;;
esac
