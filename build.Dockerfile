# Build the Linux .so for the CPA Docker image.
#
#   docker build -f build.Dockerfile -t autoclaw2api-build .
#   docker run --rm -v "$PWD/dist:/out" autoclaw2api-build
#
# The result is dist/plugins/linux/amd64/autoclaw2api.so.
FROM golang:1.24-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN mkdir -p dist/plugins/linux/amd64 \
    && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
       go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
       -o dist/plugins/linux/amd64/autoclaw2api.so . \
    && rm -f dist/plugins/linux/amd64/autoclaw2api.h

CMD ["sh", "-c", "mkdir -p /out/plugins/linux/amd64 && cp -f dist/plugins/linux/amd64/autoclaw2api.so /out/plugins/linux/amd64/"]
