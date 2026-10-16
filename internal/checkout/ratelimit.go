package checkout

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a small fixed-window limiter keyed per organisation (falls back to IP),
// stopping a script from creating thousands of Checkout Sessions.
// PRODUCTION: put WAF / Redis-based limiting in front; this is a last line of defence.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.hits[key]
	if !ok || now.After(b.reset) {
		if len(l.hits) > 10_000 {
			l.hits = map[string]*bucket{}
		}
		b = &bucket{reset: now.Add(l.window)}
		l.hits[key] = b
	}
	b.count++
	return b.count <= l.limit, time.Until(b.reset)
}

func (l *rateLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if id := IdentityFrom(r.Context()); id != nil {
			key = id.OrgID
		} else if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			key = ip // PRODUCTION behind a load balancer: use the client IP header you trust
		}
		if ok, retry := l.allow(key); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
			writeError(w, r, &APIError{Status: 429, Code: "rate_limited", Retryable: true,
				Message: "Too many attempts. Please wait a minute and try again."})
			return
		}
		next(w, r)
	}
}
