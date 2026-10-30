package httpapi

import (
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	rateLimitIdleTTL       = 10 * time.Minute
	rateLimitSweepInterval = time.Minute
)

type ipBucket struct {
	tokens   int
	fraction float64
	lastSeen time.Time
}

type rateLimiter struct {
	mu          sync.Mutex
	buckets     map[netip.Addr]*ipBucket
	rps         int
	burst       int
	now         func() time.Time
	nextCleanup time.Time
	next        http.Handler
}

// newRateLimiter requires positive rps and burst from the validated configuration.
func newRateLimiter(next http.Handler, rps, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[netip.Addr]*ipBucket),
		rps:     rps,
		burst:   burst,
		now:     time.Now,
		next:    next,
	}
}

func (l *rateLimiter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		l.next.ServeHTTP(w, r)
		return
	}

	allowed, retryAfter := l.allow(remoteIP(r.RemoteAddr))
	if allowed {
		l.next.ServeHTTP(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	if _, err := io.WriteString(w, "{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"too many requests\"}}\n"); err != nil {
		slog.ErrorContext(r.Context(), "write rate limit response", "error", err)
	}
}

func (l *rateLimiter) allow(ip netip.Addr) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Read the clock after locking so concurrent requests cannot reorder timestamps.
	now := l.now()
	if !now.Before(l.nextCleanup) {
		for key, bucket := range l.buckets {
			elapsed := now.Sub(bucket.lastSeen)
			// Eviction must not grant tokens that have not yet been replenished.
			if elapsed >= rateLimitIdleTTL && l.refilledTokens(bucket, elapsed) >= float64(l.burst-bucket.tokens) {
				delete(l.buckets, key)
			}
		}

		l.nextCleanup = now.Add(rateLimitSweepInterval)
	}

	bucket, ok := l.buckets[ip]
	if !ok {
		bucket = &ipBucket{tokens: l.burst, lastSeen: now}
		l.buckets[ip] = bucket
	}

	if now.After(bucket.lastSeen) {
		gained := l.refilledTokens(bucket, now.Sub(bucket.lastSeen))
		if gained >= float64(l.burst-bucket.tokens) {
			bucket.tokens = l.burst
			bucket.fraction = 0
		} else {
			whole := int(gained)
			bucket.tokens += whole
			bucket.fraction = gained - float64(whole)
		}

		bucket.lastSeen = now
	}

	if bucket.tokens > 0 {
		bucket.tokens--
		return true, 0
	}

	retryAfter := max(1, int(math.Ceil((1-bucket.fraction)/float64(l.rps))))
	return false, retryAfter
}

func (l *rateLimiter) refilledTokens(bucket *ipBucket, elapsed time.Duration) float64 {
	return bucket.fraction + elapsed.Seconds()*float64(l.rps)
}

func remoteIP(remoteAddr string) netip.Addr {
	addr, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		// Malformed addresses share one bucket instead of bypassing the limiter.
		return netip.Addr{}
	}

	return addr.Addr().Unmap()
}
