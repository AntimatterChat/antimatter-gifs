// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/api"
	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/media"
)

// statsRetention is how long share and search statistics are kept.
const statsRetention = 90 * 24 * time.Hour

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	st := storageFlags(fs)
	listen := fs.String("listen", env("LISTEN", ":8080"), "listen address (AM_GIFS_LISTEN)")
	publicURL := fs.String("public-url", env("PUBLIC_URL", ""), "external URL of the service; derived from requests when empty (AM_GIFS_PUBLIC_URL)")
	mediaURL := fs.String("media-url", env("MEDIA_URL", ""), "base URL of the media files when served by a CDN or object storage (AM_GIFS_MEDIA_URL)")
	keys := fs.String("api-keys", env("API_KEYS", ""), "comma-separated API keys; no key is required when empty (AM_GIFS_API_KEYS)")
	keysFile := fs.String("api-keys-file", env("API_KEYS_FILE", ""), "file with one API key per line (AM_GIFS_API_KEYS_FILE)")
	origins := fs.String("cors-origins", env("CORS_ORIGINS", "*"), "comma-separated origins allowed to call the API from browsers, * for all (AM_GIFS_CORS_ORIGINS)")
	rate := fs.Float64("rate-limit", envFloat("RATE_LIMIT", 20), "API requests per second per client, 0 to disable (AM_GIFS_RATE_LIMIT)")
	burst := fs.Int("rate-burst", int(envFloat("RATE_BURST", 60)), "API request bursts per client (AM_GIFS_RATE_BURST)")
	trustProxy := fs.Bool("trust-proxy", envBool("TRUST_PROXY", false), "use the X-Forwarded-* headers of a reverse proxy (AM_GIFS_TRUST_PROXY)")
	uploadKeys := fs.String("upload-keys", env("UPLOAD_KEYS", ""), "comma-separated keys of the clients allowed to upload GIFs and stickers; no uploads when empty (AM_GIFS_UPLOAD_KEYS)")
	uploadKeysFile := fs.String("upload-keys-file", env("UPLOAD_KEYS_FILE", ""), "file with one upload key per line (AM_GIFS_UPLOAD_KEYS_FILE)")
	uploadMaxMB := fs.Int("upload-max-mb", int(envFloat("UPLOAD_MAX_MB", api.DefaultUploadMaxBytes>>20)), "largest uploaded file in MB (AM_GIFS_UPLOAD_MAX_MB)")
	fps := fs.Int("fps", 20, "frame rate of uploaded SVG stickers")
	fs.Parse(args)

	store, err := st.open()
	if err != nil {
		return err
	}
	defer store.Close()

	apiKeys := splitComma(*keys)
	if *keysFile != "" {
		fileKeys, err := readKeys(*keysFile)
		if err != nil {
			return err
		}
		apiKeys = append(apiKeys, fileKeys...)
	}
	if len(apiKeys) == 0 {
		slog.Warn("no API key configured: the API is open to anyone who can reach it")
	}
	upKeys := splitComma(*uploadKeys)
	if *uploadKeysFile != "" {
		fileKeys, err := readKeys(*uploadKeysFile)
		if err != nil {
			return err
		}
		upKeys = append(upKeys, fileKeys...)
	}
	var uploads *importer.Importer
	if len(upKeys) > 0 {
		tools := media.DetectTools(ctx)
		slog.Info("uploads are on", "media tools", tools.String())
		uploads = &importer.Importer{
			Store:    store,
			MediaDir: st.mediaDir(),
			Tools:    tools,
			Options:  media.Options{FPS: *fps, Logger: slog.Default()},
			Logger:   slog.Default(),
		}
	}
	n, err := store.Ping(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		slog.Warn("the index is empty: import GIFs and stickers (antimatter-gifs stickers, import, fetch-tenor)")
	}

	srv := api.New(store, api.Config{
		PublicURL:   *publicURL,
		MediaURL:    *mediaURL,
		MediaDir:    st.mediaDir(),
		APIKeys:     apiKeys,
		CORSOrigins: splitComma(*origins),
		RateLimit:   *rate,
		RateBurst:   *burst,
		TrustProxy:  *trustProxy,
		Logger:      slog.Default(),

		UploadKeys:     upKeys,
		Importer:       uploads,
		UploadMaxBytes: int64(*uploadMaxMB) << 20,
	})
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go pruneStats(ctx, store)

	errc := make(chan error, 1)
	go func() {
		slog.Info("serving", "address", *listen, "posts", n, "media", st.mediaDir())
		errc <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// pruneStats drops old statistics once a day.
func pruneStats(ctx context.Context, store *index.Store) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if err := store.PruneStats(ctx, time.Now().Add(-statsRetention)); err != nil && ctx.Err() == nil {
			slog.Warn("pruning statistics failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func readKeys(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return keys, nil
}
