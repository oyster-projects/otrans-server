package server

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rateLimitEntry struct {
	started time.Time
	count   int
}

type rateLimiter struct {
	mu          sync.Mutex
	entries     map[string]rateLimitEntry
	lastCleanup time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{entries: make(map[string]rateLimitEntry), lastCleanup: time.Now()}
}

func (l *rateLimiter) allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastCleanup) >= 5*time.Minute {
		for candidate, entry := range l.entries {
			if now.Sub(entry.started) >= 10*time.Minute {
				delete(l.entries, candidate)
			}
		}
		l.lastCleanup = now
	}
	entry := l.entries[key]
	if entry.started.IsZero() || now.Sub(entry.started) >= window {
		entry = rateLimitEntry{started: now}
	}
	entry.count++
	l.entries[key] = entry
	remaining := window - now.Sub(entry.started)
	return entry.count <= limit, remaining
}

func (s *Server) enforceRateLimit(w http.ResponseWriter, r *http.Request, scope string, limit int, window time.Duration) bool {
	allowed, retryAfter := s.limiter.allow(scope+"|"+clientAddress(r), limit, window)
	if allowed {
		return true
	}
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "too many requests; retry later")
	return false
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-site")
		if strings.HasPrefix(r.URL.Path, "/admin/") &&
			strings.EqualFold(r.Header.Get("Sec-Fetch-Dest"), "iframe") &&
			strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			writeError(w, http.StatusForbidden, "cross-site framing is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setReadDeadline(w http.ResponseWriter, duration time.Duration) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(duration))
}

func setWriteDeadline(w http.ResponseWriter, duration time.Duration) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(duration))
}

func transferDeadline(size int64) time.Duration {
	const minimumRate = int64(128 * 1024)
	duration := 2*time.Minute + time.Duration(size/minimumRate)*time.Second
	if duration > 12*time.Hour {
		return 12 * time.Hour
	}
	return duration
}
