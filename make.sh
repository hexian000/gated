#!/bin/sh -e

cd "$(dirname "$0")"
mkdir -p build

VERSION=""
if git rev-parse --git-dir >/dev/null 2>&1; then
    VERSION="$(git tag --points-at HEAD)"
    if [ -z "${VERSION}" ]; then
        VERSION="git-$(git rev-parse --short HEAD)"
    fi
    if ! git diff-index --quiet HEAD --; then
        VERSION="${VERSION}+"
    fi
fi
echo "+ version: ${VERSION}"

GOFLAGS="-trimpath"
LDFLAGS="-s -w"
if [ -n "${VERSION}" ]; then
    LDFLAGS="${LDFLAGS} -X github.com/hexian000/gated/version.tag=${VERSION}"
fi

export CGO_ENABLED=0

case "$1" in
"x")
    # cross build for all supported targets
    # not supported targets are likely to work
    set -x
    go mod vendor
    GOOS="linux" GOARCH="mipsle" GOMIPS="softfloat" nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated.linux-mipsle cmd/gated/main.go
    GOOS="linux" GOARCH="arm" GOARM=7 nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated.linux-armv7 cmd/gated/main.go
    GOOS="linux" GOARCH="arm64" nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated.linux-arm64 cmd/gated/main.go
    GOOS="linux" GOARCH="amd64" nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated.linux-amd64 cmd/gated/main.go
    GOOS="windows" GOARCH="amd64" nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated.windows-amd64.exe cmd/gated/main.go
    ;;
*)
    # build for native system only
    set -x
    go mod vendor
    nice go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o build/gated cmd/gated/main.go
    ;;
esac
