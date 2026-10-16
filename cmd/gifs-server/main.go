// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Command gifs-server runs the GIF and sticker service and manages its catalogue.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antimatterchat/gifs-server/internal/index"
)

const usage = `gifs-server: a self-hosted GIF and sticker service with the Tenor API v2.

Usage:
  gifs-server serve [flags]               run the service
  gifs-server import [flags] FILE...      import GIFs or stickers (-sticker)
  gifs-server import -manifest FILE       import a manifest (see README)
  gifs-server stickers [-pack FILE]       import a sticker pack (default stickers/pack.json)
  gifs-server fetch-tenor [flags]         download a starter dataset from Tenor (TENOR_API_KEY)
  gifs-server delete ID...                delete posts
  gifs-server healthcheck [-url URL]      check a running service (for containers)
  gifs-server version                     print the version

Run "gifs-server COMMAND -h" for the flags of a command. Flags default to AM_GIFS_*
environment variables, e.g. AM_GIFS_DATA_DIR for -data.
`

// version and commit are set at build time by the release build (-ldflags "-X main.version=...").
var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	if commit == "" {
		return "gifs-server " + version
	}
	return "gifs-server " + version + " (" + commit + ")"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := map[string]func(context.Context, []string) error{
		"serve":       serve,
		"import":      importFiles,
		"stickers":    importStickers,
		"fetch-tenor": fetchTenor,
		"delete":      deletePosts,
		"healthcheck": healthcheck,
		"version": func(context.Context, []string) error {
			fmt.Println(versionString())
			return nil
		},
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		if os.Args[1] != "-h" && os.Args[1] != "--help" && os.Args[1] != "help" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := cmd(ctx, os.Args[2:]); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

// env returns the environment variable AM_GIFS_<name>, or def.
func env(name, def string) string {
	if v, ok := os.LookupEnv("AM_GIFS_" + name); ok {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if f, err := strconv.ParseFloat(env(name, ""), 64); err == nil {
		return f
	}
	return def
}

func envBool(name string, def bool) bool {
	if b, err := strconv.ParseBool(env(name, "")); err == nil {
		return b
	}
	return def
}

// storage are the flags locating the index and the media files.
type storage struct {
	data, db, media, logLevel *string
}

func storageFlags(fs *flag.FlagSet) storage {
	return storage{
		data:     fs.String("data", env("DATA_DIR", "data"), "data directory, holding index.db and media/ by default (AM_GIFS_DATA_DIR)"),
		db:       fs.String("db", env("DB", ""), "index database file (AM_GIFS_DB, default DATA/index.db)"),
		media:    fs.String("media", env("MEDIA_DIR", ""), "media directory (AM_GIFS_MEDIA_DIR, default DATA/media)"),
		logLevel: fs.String("log-level", env("LOG_LEVEL", "info"), "debug, info, warn or error (AM_GIFS_LOG_LEVEL)"),
	}
}

func (s storage) dbPath() string {
	if *s.db != "" {
		return *s.db
	}
	return filepath.Join(*s.data, "index.db")
}

func (s storage) mediaDir() string {
	if *s.media != "" {
		return *s.media
	}
	return filepath.Join(*s.data, "media")
}

// open sets up logging and opens the index, creating the directories.
func (s storage) open() (*index.Store, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(*s.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", *s.logLevel)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	for _, dir := range []string{filepath.Dir(s.dbPath()), s.mediaDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return index.Open(s.dbPath())
}

func splitComma(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// healthcheck exits with an error when the service doesn't answer its health check; it lets
// container images without curl or wget have a HEALTHCHECK.
func healthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	listen := env("LISTEN", ":8080")
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	target := fs.String("url", "http://"+listen+"/healthz", "health check URL")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: %s", resp.Status)
	}
	return nil
}
