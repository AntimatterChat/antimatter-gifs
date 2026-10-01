# antimatter-gifs: the GIF and sticker service.
#
#   docker build -t antimatter-gifs .
#   docker run -d -p 8080:8080 -v gifs-data:/data -e AM_GIFS_API_KEYS=changeme antimatter-gifs
#   docker run --rm -v gifs-data:/data antimatter-gifs stickers   # import the sticker pack

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/antimatter-gifs ./cmd/antimatter-gifs

FROM alpine:3.22
# ffmpeg, gifsicle and rsvg-convert generate the GIF, video and sticker formats at import; the
# service itself doesn't need them. The fonts draw the sticker labels.
RUN apk add --no-cache ffmpeg gifsicle rsvg-convert font-dejavu fontconfig ca-certificates \
    && fc-cache -f \
    && adduser -D -H -u 10001 gifs \
    && mkdir /data && chown gifs /data
COPY --from=build /out/antimatter-gifs /usr/local/bin/antimatter-gifs
COPY stickers /usr/share/antimatter-gifs/stickers
ENV AM_GIFS_DATA_DIR=/data \
    AM_GIFS_LISTEN=:8080 \
    AM_GIFS_STICKER_PACK=/usr/share/antimatter-gifs/stickers/pack.json \
    XDG_CACHE_HOME=/tmp
USER gifs
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["antimatter-gifs", "healthcheck"]
ENTRYPOINT ["antimatter-gifs"]
CMD ["serve"]
