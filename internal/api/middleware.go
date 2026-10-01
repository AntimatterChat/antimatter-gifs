// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"crypto/subtle"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// apiKey returns the API key of the request: the key parameter, or the X-Goog-Api-Key header like
// Google APIs.
func apiKey(r *http.Request) string {
	if k := r.URL.Query().Get("key"); k != "" {
		return k
	}
	return r.Header.Get("X-Goog-Api-Key")
}

// requireKey rejects requests without a configured API key, with the errors of Google APIs.
func (s *Server) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		key := apiKey(r)
		if key == "" {
			writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "The request is missing a valid API key.")
			return
		}
		valid := 0
		for _, k := range s.keys {
			valid |= subtle.ConstantTimeCompare(k, []byte(key))
		}
		if valid != 1 {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "API key not valid. Please pass a valid API key.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors answers preflight requests and allows the configured origins.
func (s *Server) cors(next http.Handler) http.Handler {
	wildcard := slices.Contains(s.cfg.CORSOrigins, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && (wildcard || slices.Contains(s.cfg.CORSOrigins, origin))
		if allowed {
			h := w.Header()
			if wildcard {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, X-Goog-Api-Key")
				h.Set("Access-Control-Max-Age", "86400")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit limits the requests of each client (API key and IP address).
func (s *Server) rateLimit(next http.Handler) http.Handler {
	if s.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retry := s.limiter.allow(s.clientIP(r)+"|"+apiKey(r), s.cfg.Now())
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Too many requests, slow down.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := firstHeaderValue(r, "X-Forwarded-For"); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter is a token bucket per client.
type limiter struct {
	mu        sync.Mutex
	rate      float64
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes a token from the client's bucket, or tells how long to wait for one.
func (l *limiter) allow(client string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b := l.buckets[client]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[client] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// sweep forgets, once a minute, the buckets that have refilled.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// accessLog logs requests without their query, which holds the API key and search terms.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "INTERNAL", "Internal error.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
