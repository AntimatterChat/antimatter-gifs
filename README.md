# antimatter-gifs

A self-hosted GIF and sticker service for Antimatter that speaks the
[Tenor API v2](https://developers.google.com/tenor/guides/endpoints), so existing Tenor clients can
use it by changing their base URL (`https://tenor.googleapis.com/v2` → `https://gifs.example.com/v2`).
The Antimatter GIFs plugin (`antimatter-plugin-gifs`) uses it for its GIF and sticker picker.

- **Tenor API v2**: `search`, `featured`, `categories`, `search_suggestions`, `autocomplete`,
  `trending_terms`, `registershare` and `posts`, with Tenor's parameters and JSON (see below).
- **Your own catalogue**: a SQLite index (pure Go, no cgo) with full-text search over titles, tags
  and descriptions, curated categories, sticker packs, and trending GIFs and terms from shares and
  searches.
- **Stickers**: `searchfilter=sticker` and the transparent formats (`gif_transparent`,
  `webp_transparent` and their tiny and nano sizes). An original pack of 24 animated stickers comes
  with the service ([stickers/](stickers/), CC BY 4.0).
- **Media** served by the service or by a CDN/object storage, in every Tenor format, generated at
  import with ffmpeg when available.
- API keys, CORS, rate limiting, a health endpoint and a Docker image.

## Quick start

With Docker (see [Docker image](#docker-image)):

```sh
docker volume create gifs-data
docker run --rm -v gifs-data:/data ghcr.io/antimatterchat/antimatter-gifs stickers   # import the sticker pack
docker run -d --name gifs -p 8080:8080 -v gifs-data:/data \
    -e AM_GIFS_API_KEYS=change-me ghcr.io/antimatterchat/antimatter-gifs
curl 'http://localhost:8080/v2/featured?key=change-me&searchfilter=sticker&limit=2'
```

From the sources (Go 1.25+; ffmpeg, gifsicle and rsvg-convert are optional, see
[Formats](#formats)):

```sh
go build -o bin/antimatter-gifs ./cmd/antimatter-gifs
bin/antimatter-gifs stickers                  # imports stickers/pack.json into ./data
bin/antimatter-gifs import -tags cat,happy -category cats ~/Pictures/happy-cat.gif
bin/antimatter-gifs serve                     # http://localhost:8080, no API key needed
```

Everything lives in the data directory (`./data`, `/data` in the image): `index.db` and `media/`.
Back it up as a whole.

### Using it from Antimatter

Install the GIFs plugin and set, in **System Console > Plugins > GIFs**:

- **GIF service URL**: the service's API base, e.g. `http://antimatter-gifs:8080/v2`. Only the
  Antimatter server talks to the service (the plugin proxies the API and the media), so the service
  doesn't need to be public.
- **API key**: one of `AM_GIFS_API_KEYS`.

## Docker image

`ghcr.io/antimatterchat/antimatter-gifs`, for `linux/amd64` and `linux/arm64`. Tags: `latest`
(the `antimatter` branch), `X.Y.Z` and `X.Y` (releases, from `vX.Y.Z` tags) and `sha-<commit>`.
Pin a version in production. `docker build -t antimatter-gifs .` builds the same image locally.

- **Command**: the entrypoint is `antimatter-gifs` and the default command `serve`, so
  `docker run IMAGE` runs the service and `docker run IMAGE <command>` runs any other command
  (`stickers`, `import`, `fetch-tenor`, `delete`, `healthcheck`) on the same data.
- **Port** 8080 (the API, `/media/` and `/healthz`).
- **Volume** `/data`: the index and the media, everything to back up. The service runs as the
  unprivileged user `gifs` (uid 10001); a named volume gets the right owner automatically, a bind
  mount needs `chown -R 10001 <dir>` first. The image also runs with a read-only root filesystem
  (`--read-only --tmpfs /tmp`).
- **Environment**: any variable of [Configuration](#configuration). The image sets
  `AM_GIFS_DATA_DIR=/data`, `AM_GIFS_LISTEN=:8080` and `AM_GIFS_STICKER_PACK` (the bundled pack).
- **Health check**: `antimatter-gifs healthcheck` against `/healthz` (no curl needed), so
  `docker ps` and compose's `service_healthy` work.
- **Media tools**: ffmpeg, gifsicle, rsvg-convert and fonts, for imports and uploads.

**First run.** A new volume has an empty catalogue: import the bundled sticker pack once (about a
minute and a half; the sticker files and `pack.json` are in the image under
`/usr/share/antimatter-gifs/stickers`):

```sh
docker run --rm -v gifs-data:/data ghcr.io/antimatterchat/antimatter-gifs stickers
```

Running it again updates the stickers in place (e.g. after an upgrade that changes the pack). It
can run while the service is up. Import your own GIFs the same way, with their directory mounted:

```sh
docker run --rm -v gifs-data:/data -v "$PWD/gifs:/import:ro" ghcr.io/antimatterchat/antimatter-gifs \
    import -manifest /import/manifest.json
```

**API keys.** Without `AM_GIFS_API_KEYS` the API is open to anyone who can reach it: always set
a key, even when only Antimatter can reach the service. Generate one with `openssl rand -hex 32`,
give it to the service and to the GIFs plugin (*API key*); several keys, comma-separated, let you
rotate them. Upload keys (`AM_GIFS_UPLOAD_KEYS`) are separate and only for the plugin's *Upload
key*. With Docker secrets, use the `_FILE` variants (`AM_GIFS_API_KEYS_FILE=/run/secrets/...`).

A complete deployment with Antimatter, PostgreSQL and nginx, where the service is only reachable by
the Antimatter server, is in
[antimatter-docker](https://github.com/AntimatterChat/antimatter-docker/tree/main/deploy/compose).

## Configuration

`antimatter-gifs serve` flags, which default to environment variables:

| Flag | Variable | Default | Meaning |
| --- | --- | --- | --- |
| `-data` | `AM_GIFS_DATA_DIR` | `data` | Data directory. |
| `-db` | `AM_GIFS_DB` | `DATA/index.db` | Index file. |
| `-media` | `AM_GIFS_MEDIA_DIR` | `DATA/media` | Media directory, served under `/media/`. |
| `-listen` | `AM_GIFS_LISTEN` | `:8080` | Listen address. |
| `-public-url` | `AM_GIFS_PUBLIC_URL` | from requests | External URL, used in media and item URLs. |
| `-media-url` | `AM_GIFS_MEDIA_URL` | `PUBLIC_URL/media/` | Base URL of the media when a CDN or object storage serves them (see below). |
| `-api-keys` | `AM_GIFS_API_KEYS` | none | Comma-separated API keys. **Without keys, the API is open.** |
| `-api-keys-file` | `AM_GIFS_API_KEYS_FILE` | | A file with one key per line (`#` comments). |
| `-cors-origins` | `AM_GIFS_CORS_ORIGINS` | `*` | Origins allowed to call the API from browsers. |
| `-rate-limit` | `AM_GIFS_RATE_LIMIT` | `20` | API requests per second per client (API key + IP); `0` disables. |
| `-rate-burst` | `AM_GIFS_RATE_BURST` | `60` | Burst size per client. |
| `-trust-proxy` | `AM_GIFS_TRUST_PROXY` | `false` | Use `X-Forwarded-For/-Proto/-Host` (behind a reverse proxy only). |
| `-upload-keys` | `AM_GIFS_UPLOAD_KEYS` | none | Comma-separated keys of the clients allowed to upload GIFs and stickers (see [Uploads](#uploads)). No uploads without keys. |
| `-upload-keys-file` | `AM_GIFS_UPLOAD_KEYS_FILE` | | A file with one upload key per line. |
| `-upload-max-mb` | `AM_GIFS_UPLOAD_MAX_MB` | `16` | Largest uploaded file. |
| `-log-level` | `AM_GIFS_LOG_LEVEL` | `info` | `debug` logs every request (path and status only: queries hold keys and search terms). |

The key is read from the `key` parameter, like Tenor, or the `X-Goog-Api-Key` header. Missing and
invalid keys get Google's `403 PERMISSION_DENIED` and `400 INVALID_ARGUMENT` errors; too many
requests get `429 RESOURCE_EXHAUSTED` with `Retry-After`. Media, `/view/{id}` and `/healthz` need no
key and aren't rate limited.

**Object storage.** Media paths are stable (`<last two digits of the id>/<id>/<format>.<ext>`), so the
media directory can be mirrored to a bucket or CDN (`rclone sync data/media s3:bucket/gifs`, after
each import) and served from there with `AM_GIFS_MEDIA_URL=https://cdn.example.com/gifs/`.

## API

Base path `/v2`. Requests and responses follow Tenor's documentation:

| Endpoint | Parameters | Response |
| --- | --- | --- |
| `GET /v2/search` | `q` (required), `limit` (1–50, default 20), `pos`, `searchfilter`, `contentfilter`, `media_filter`, `ar_range`, `random`, `locale` | `{"results": [RESPONSE_OBJECT], "next": "…"}` |
| `GET /v2/featured` | same, without `q` | same; curated order, then shares of the week |
| `GET /v2/posts` | `ids` (comma-separated, ≤ 50), `media_filter` | `{"results": [RESPONSE_OBJECT]}` |
| `GET /v2/categories` | `type` (`featured` or `trending`), `contentfilter`, `locale` | `{"locale", "tags": [{"searchterm", "path", "image", "name"}]}` |
| `GET /v2/search_suggestions` | `q`, `limit` | `{"locale", "results": ["term"]}` |
| `GET /v2/autocomplete` | `q`, `limit` | `{"locale", "results": ["term"]}` |
| `GET /v2/trending_terms` | `limit` | `{"locale", "results": ["term"]}` |
| `GET or POST /v2/registershare` | `id`, `q` | `{}`; counts a share for trending |

- **Response objects** have `id`, `title`, `media_formats` (`{format: {url, dims, duration,
  size}}`), `created`, `content_description`, `itemurl` and `url` (the `/view/{id}` page), `tags`,
  `flags` (`sticker`, `static`), `hasaudio`, `hascaption` and `bg_color`. Third-party posts add an
  `attribution` object (`source`, `source_id`, `url`, `text`), which Tenor clients ignore.
- **`searchfilter`**: `sticker` for stickers (GIFs otherwise), `static` / `-static` for still /
  animated posts, comma-separated.
- **`contentfilter`**: `off` (all), `low` (G, PG, PG-13), `medium` (G, PG), `high` (G).
- **`ar_range`**: `all`, `wide` (0.42–2.36), `standard` (0.56–1.78), on the full size media.
- **`media_filter`**: formats to return (fewer formats, smaller responses); `minimal` and `basic`
  (Tenor v1 presets) also work.
- **`pos`** is the `next` value of the previous page; `next` is empty on the last page.
- **Categories**: `type=featured` returns the curated categories; with `searchfilter=sticker` (an
  extension) it returns the sticker packs. `type=trending` makes categories of the most shared terms.
  Searching for a category's `searchterm` returns its curated posts first, in order.
- **Ranking**: BM25 over title (×4), tags (×3) and description, with stemming and prefix matching,
  scaled by the share of query words matched, plus a small boost for curated score and recent shares.
- Shares and searches are only counted per post/term and day (90 days kept); nothing about users or
  clients is stored.

### Uploads

`POST /v2/upload` (not part of the Tenor API) lets a trusted client, such as the Antimatter GIFs
plugin, add GIFs and stickers. It needs one of the upload keys as `Authorization: Bearer <key>` (the
API keys don't work) and only exists when upload keys are set. The request is `multipart/form-data`:

| Field | Meaning |
| --- | --- |
| `file` (required) | GIFs: GIF, PNG (APNG), WebP, MP4 or WebM; stickers: PNG, WebP, GIF or SVG (animated with the subset of SMIL of the sticker pack). The type is read from the content. |
| `kind` | `gif` (default) or `sticker` |
| `title` (required) | Up to 100 characters. |
| `tags` | Comma-separated, up to 20 (40 characters each). |
| `description`, `attribution` | Optional texts. |
| `rating` | `g` (default), `pg`, `pg-13` or `r`. |
| `pack` | Stickers: the sticker pack (category) to add the sticker to, created if needed. |

The file is converted like imported files (one upload at a time) and the answer is the new post,
`{"results": [RESPONSE_OBJECT]}`, with all its formats. Uploading the same file again updates its
post. Errors: `400` (invalid field or file type), `403` (missing or invalid key), `413` (file too
large), `422` (the file can't be converted).

Other routes: `GET /media/...` (media files, cacheable, sandboxed by a CSP), `GET /view/{id}` (a page
per post, with attribution), `GET /healthz` (`{"status": "ok", "posts": n}`, `503` when the index
is unavailable).

## Catalogue

### Importing

```sh
antimatter-gifs import [-sticker] [-title T] [-tags a,b] [-description D] [-rating g|pg|pg13|r] \
    [-category TERM] [-source S -source-url URL -attribution TEXT] FILE...
antimatter-gifs import -manifest manifest.json
antimatter-gifs stickers [-pack stickers/pack.json]
antimatter-gifs delete ID...
```

Re-importing the same file (same content) or the same source item updates the existing post.
`-category` appends the files to a category (created when missing). The title defaults to the file
name; tags and description are what search looks at.

A **manifest** imports many items with their categories:

```json
{
  "kind": "gif",
  "source": "my-artist",
  "attribution": "By My Artist, CC BY 4.0",
  "items": [
    {"key": "wave", "file": "wave.mp4", "title": "Wave", "tags": ["hello", "hi"], "rating": "g", "score": 10},
    {"key": "yes", "source_id": "yes-1", "title": "Yes!", "media": {
      "gif": {"file": "yes.gif"}, "tinygif": {"file": "yes-small.gif", "dims": [220, 124]}}}
  ],
  "categories": [{"name": "#hello", "searchterm": "hello", "items": ["wave", "yes"], "image": "wave"}]
}
```

`file` is converted to the formats below; `media` gives ready-made files per format instead.
Paths are relative to the manifest. `score` orders the featured list.

### Formats

When the tools are installed, an import generates the Tenor formats:

| Kind | Formats | Tools |
| --- | --- | --- |
| GIF | `gif`, `tinygif` (≤ 220 wide), `nanogif` (≤ 90 tall), `preview` (PNG) | ffmpeg |
| | `mp4` (≤ 640), `tinymp4` (≤ 320), `nanomp4` (≤ 150) | ffmpeg with libx264 |
| | `webm`, `tinywebm`, `nanowebm` | ffmpeg with libvpx(-vp9) |
| | `mediumgif` (lossy), and optimised GIFs | gifsicle |
| Sticker | `gif_transparent` (≤ 512), `tinygif_transparent` (≤ 220), `nanogif_transparent` (≤ 90), `preview` | ffmpeg |
| | `webp_transparent`, `tinywebp_transparent`, `nanowebp_transparent` | ffmpeg with libwebp_anim |
| | SVG stickers | rsvg-convert + ffmpeg |

All tools are optional (the Docker image has them all). Without ffmpeg, a GIF is kept as is (only
`gif` or `gif_transparent`), MP4/WebM/WebP/PNG files are kept in their own format, and other files
(and SVG stickers) can't be imported. `-no-variants` keeps the source file only, even with the tools.
Formats an ffmpeg build can't encode are skipped. Clients should fall back between formats (the
plugin does).

### Stickers

[stickers/](stickers/) is an original pack of 24 animated stickers (Lab crew, Reactions, Everyday),
drawn as animated SVG and licensed **CC BY 4.0** ([stickers/LICENSE](stickers/LICENSE)); credit:
"Antimatter stickers by the Antimatter contributors, CC BY 4.0". `antimatter-gifs stickers` renders
them to transparent GIF and WebP (20 fps, one loop) and makes each pack a sticker category.

To add a pack, add SVG files and a pack to `pack.json` (or make your own `pack.json`). The
renderer understands a subset of SMIL: self-closing `<animateTransform>` (translate, scale, rotate)
and `<animate attributeName="opacity">` as the first children of a `<g>` without a transform or
opacity of its own, with `values`, `keyTimes`, `calcMode` linear or spline, `additive="sum"` and one
`dur` per sticker. Anything else is reported as an error.

### A starter dataset from Tenor

`fetch-tenor` downloads the featured GIFs, the GIFs of the first featured categories and the
featured stickers from the official Tenor API with **your own** API key, and imports them:

```sh
export TENOR_API_KEY=...     # https://developers.google.com/tenor/guides/quickstart
antimatter-gifs fetch-tenor -featured 200 -categories 12 -per-category 12 -stickers 48 -contentfilter medium
```

- Files go to `DATA/tenor/` with a `manifest.json` (`-no-import` to stop there, `-out` for another
  directory); downloads resume. Posts keep Tenor's order (featured score), categories and metadata,
  and their Tenor id and URL as source and attribution (`Via Tenor`).
- Posts are rated after the content filter they were fetched with (`high` → G, `medium` → PG,
  `low` → PG-13, `off` → R), so the service's `contentfilter` stays meaningful.
- **This is third-party content.** It is never committed (the data directory is ignored by git).
  Caching and redistributing Tenor content is governed by the
  Tenor API terms of service (see https://developers.google.com/tenor) and the rights of the content's
  owners: check that your use is allowed before serving it.

## Development

```sh
go test ./...        # the media tests use ffmpeg and rsvg-convert when installed
go vet ./...
```

The *Build the image* workflow (`.github/workflows/image.yml`) runs the tests and then publishes
the image: on pushes to `antimatter` (`latest`), on `v*` tags (the version) and on manual runs
(optional extra tag). Pull requests only build it. Make the package public in the organisation's
package settings for anonymous pulls.

Layout: `cmd/antimatter-gifs` (commands), `internal/api` (HTTP API), `internal/index` (SQLite
index and search), `internal/importer` (manifests, sticker packs), `internal/media` (formats),
`internal/svgframes` (SVG animation frames), `internal/tenor` (Tenor dataset fetcher).

## License

GNU Affero General Public License v3.0, see [LICENSE](LICENSE). The sticker artwork in
[stickers/](stickers/) is licensed CC BY 4.0. The service depends on modernc.org/sqlite (BSD-3-Clause).
