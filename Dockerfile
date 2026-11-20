FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go ./
RUN CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torrent2webdav .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/torrent2webdav /usr/local/bin/torrent2webdav
ENV CACHE_DIR=/cache \
    WEBDAV_ADDR=:8080 \
    TORRENT_PORT=42069
VOLUME ["/cache", "/torrents"]
EXPOSE 8080/tcp 42069/tcp 42069/udp
ENTRYPOINT ["torrent2webdav"]
