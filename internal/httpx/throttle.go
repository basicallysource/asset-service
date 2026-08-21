package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Throttle limits how often one caller may do something expensive, like asking
// another service for a device code. It is a token bucket per key, held in
// memory: this service runs as one process, and a limiter that survived a
// restart would be more machinery than the problem deserves.
type Throttle struct {
	// Every is how often one token is replenished; Burst is how many may be
	// spent at once.
	Every time.Duration
	Burst int

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// Allow reports whether this key may proceed, and spends a token if so.
func (t *Throttle) Allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.buckets == nil {
		t.buckets = map[string]*bucket{}
		t.swept = now
	}
	t.sweep(now)

	b, ok := t.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(t.Burst), seen: now}
		t.buckets[key] = b
	}

	b.tokens += now.Sub(b.seen).Seconds() / t.Every.Seconds()
	if b.tokens > float64(t.Burst) {
		b.tokens = float64(t.Burst)
	}
	b.seen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been full for long enough to be indistinguishable
// from new ones, so the map cannot grow forever.
func (t *Throttle) sweep(now time.Time) {
	idle := t.Every * time.Duration(t.Burst) * 2
	if now.Sub(t.swept) < idle {
		return
	}
	for key, b := range t.buckets {
		if now.Sub(b.seen) > idle {
			delete(t.buckets, key)
		}
	}
	t.swept = now
}

// ClientIP is who to hold responsible for a request.
//
// header names the one header a proxy in front of this service sets to the
// real client address; it is only consulted when an operator has said which
// header that is. Trusting such a header by default would let any caller
// choose their own identity by sending it themselves.
func ClientIP(r *http.Request, header string) string {
	if header != "" {
		if v := r.Header.Get(header); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
