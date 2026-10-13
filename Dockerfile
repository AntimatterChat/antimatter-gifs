# syntax=docker/dockerfile:1
#
# gifs-server: the GIF and sticker service.
#
#   docker build -t gifs-server .
#   docker run -d -p 8080:8080 -v gifs-data:/data -e AM_GIFS_API_KEYS=changeme gifs-server
#   docker run --rm -v gifs-data:/data gifs-server stickers   # import the sticker pack

# The binary is pure Go: build it on the build machine's platform and cross-compile it for the
# target one, so multi-platform builds don't run the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/gifs-server ./cmd/gifs-server

FROM alpine:3.22
LABEL org.opencontainers.image.title="gifs-server" \
      org.opencontainers.image.description="Self-hosted GIF and sticker server with the Tenor API v2" \
      org.opencontainers.image.source="https://github.com/AntimatterChat/gifs-server" \
      org.opencontainers.image.licenses="AGPL-3.0-only AND CC-BY-4.0"
# ffmpeg, gifsicle and rsvg-convert generate the GIF, video and sticker formats at import; the
# service itself doesn't need them. The fonts draw the sticker labels.
RUN apk add --no-cache ffmpeg gifsicle rsvg-convert font-dejavu fontconfig ca-certificates \
    && fc-cache -f \
    && adduser -D -H -u 10001 gifs \
    && mkdir /data && chown gifs /data
COPY --from=build /out/gifs-server /usr/local/bin/gifs-server
COPY stickers /usr/share/gifs-server/stickers
ENV AM_GIFS_DATA_DIR=/data \
    AM_GIFS_LISTEN=:8080 \
    AM_GIFS_STICKER_PACK=/usr/share/gifs-server/stickers/pack.json \
    XDG_CACHE_HOME=/tmp
USER gifs
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --start-interval=2s \
    CMD ["gifs-server", "healthcheck"]
ENTRYPOINT ["gifs-server"]
CMD ["serve"]
