package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestRateLimiterTokenBucket(t *testing.T) {
	base := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	now := base
	limiter := newRateLimiter(nil, 2, 2)
	limiter.now = func() time.Time { return now }
	ip := netip.MustParseAddr("192.0.2.1")

	for _, test := range []struct {
		name       string
		elapsed    time.Duration
		allowed    bool
		retryAfter int
	}{
		{"first token", 0, true, 0},
		{"second token", 0, true, 0},
		{"exhausted", 0, false, 1},
		{"fractional refill", 250 * time.Millisecond, false, 1},
		{"one token restored", 500 * time.Millisecond, true, 0},
		{"restored token spent", 500 * time.Millisecond, false, 1},
		{"fraction retained", 750 * time.Millisecond, false, 1},
		{"fractions combined", time.Second, true, 0},
		{"long idle first token", 10 * time.Second, true, 0},
		{"long idle second token", 10 * time.Second, true, 0},
		{"burst capacity respected", 10 * time.Second, false, 1},
		{"backward clock", 9 * time.Second, false, 1},
		{"no refill counted twice", 10250 * time.Millisecond, false, 1},
		{"refill after backward clock", 10500 * time.Millisecond, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = base.Add(test.elapsed)
			allowed, retryAfter := limiter.allow(ip)
			if allowed != test.allowed || retryAfter != test.retryAfter {
				t.Fatalf("allow = (%t, %d); want (%t, %d)", allowed, retryAfter, test.allowed, test.retryAfter)
			}
		})
	}
}

func TestRateLimiterLargeBurst(t *testing.T) {
	now := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	burst := int(^uint(0) >> 1)
	limiter := newRateLimiter(nil, 10, burst)
	limiter.now = func() time.Time { return now }
	ip := netip.MustParseAddr("192.0.2.1")
	for range 3 {
		if allowed, _ := limiter.allow(ip); !allowed {
			t.Fatal("initial token rejected")
		}
	}

	if got := limiter.buckets[ip].tokens; got != burst-3 {
		t.Fatalf("tokens = %d; want %d", got, burst-3)
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	base := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	now := base
	limiter := newRateLimiter(nil, 1, 1000)
	limiter.now = func() time.Time { return now }
	depletedIP := netip.MustParseAddr("192.0.2.1")
	idleIP := netip.MustParseAddr("192.0.2.2")
	activeIP := netip.MustParseAddr("192.0.2.3")
	triggerIP := netip.MustParseAddr("192.0.2.4")

	for range 1000 {
		if allowed, _ := limiter.allow(depletedIP); !allowed {
			t.Fatal("initial token rejected")
		}
	}

	limiter.allow(idleIP)
	now = base.Add(9 * time.Minute)
	limiter.allow(activeIP)
	now = base.Add(rateLimitIdleTTL)
	limiter.allow(triggerIP)

	if _, ok := limiter.buckets[idleIP]; ok {
		t.Fatal("inactive fully replenished bucket was not removed")
	}

	if bucket, ok := limiter.buckets[depletedIP]; !ok || bucket.tokens != 0 {
		t.Fatal("cleanup reset a bucket before it could fully replenish")
	}

	if _, ok := limiter.buckets[activeIP]; !ok {
		t.Fatal("active bucket was removed")
	}

	now = base.Add(1000 * time.Second)
	limiter.allow(triggerIP)
	if _, ok := limiter.buckets[depletedIP]; ok {
		t.Fatal("inactive bucket was not removed after fully replenishing")
	}

	if _, ok := limiter.buckets[activeIP]; !ok {
		t.Fatal("bucket younger than the idle TTL was removed")
	}
}

func TestRateLimiterHTTPResponse(t *testing.T) {
	base := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	now := base
	calls := 0
	limiter := newRateLimiter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	}), 2, 1)
	limiter.now = func() time.Time { return now }

	for _, test := range []struct {
		elapsed time.Duration
		status  int
	}{
		{0, http.StatusAccepted},
		{0, http.StatusTooManyRequests},
		{250 * time.Millisecond, http.StatusTooManyRequests},
		{500 * time.Millisecond, http.StatusAccepted},
	} {
		now = base.Add(test.elapsed)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
		request.RemoteAddr = "192.0.2.1:1000"
		response := httptest.NewRecorder()
		limiter.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("status = %d; want %d", response.Code, test.status)
		}

		if test.status != http.StatusTooManyRequests {
			if response.Header().Get("Retry-After") != "" {
				t.Fatal("allowed response contains Retry-After")
			}

			continue
		}

		if response.Header().Get("Content-Type") != "application/json" ||
			response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("Retry-After") != "1" {
			t.Fatalf("unexpected 429 headers: %v", response.Header())
		}

		var payload map[string]map[string]string
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}

		if len(payload) != 1 || len(payload["error"]) != 2 ||
			payload["error"]["code"] != "rate_limit_exceeded" ||
			payload["error"]["message"] != "too many requests" {
			t.Fatalf("unexpected error envelope: %v", payload)
		}
	}

	if calls != 2 {
		t.Fatalf("downstream handler calls = %d; want 2", calls)
	}
}

func TestRateLimiterRemoteIP(t *testing.T) {
	for _, test := range []struct {
		name      string
		firstAddr string
		nextAddr  string
		headers   map[string]string
		status    int
	}{
		{"ports share quota", "192.0.2.1:1000", "192.0.2.1:2000", nil, http.StatusTooManyRequests},
		{"proxy headers ignored", "192.0.2.1:1000", "192.0.2.1:2000", map[string]string{
			"X-Forwarded-For": "192.0.2.2",
			"Forwarded":       "for=192.0.2.3",
			"X-Real-IP":       "192.0.2.4",
		}, http.StatusTooManyRequests},
		{"mapped IPv4 shares quota", "192.0.2.1:1000", "[::ffff:192.0.2.1]:2000", nil, http.StatusTooManyRequests},
		{"different IPv4 independent", "192.0.2.1:1000", "192.0.2.2:2000", map[string]string{"X-Forwarded-For": "192.0.2.1"}, http.StatusNoContent},
		{"IPv6 normalized", "[2001:0db8:0:0:0:0:0:1]:1000", "[2001:db8::1]:2000", nil, http.StatusTooManyRequests},
		{"different IPv6 independent", "[2001:db8::1]:1000", "[2001:db8::2]:2000", nil, http.StatusNoContent},
		{"malformed addresses share quota", "invalid-one", "invalid-two", nil, http.StatusTooManyRequests},
		{"empty address uses fallback", "", "invalid", nil, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
			limiter := newRateLimiter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), 1, 1)
			limiter.now = func() time.Time { return now }

			first := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?team_id=1", nil)
			first.RemoteAddr = test.firstAddr
			firstResponse := httptest.NewRecorder()
			limiter.ServeHTTP(firstResponse, first)
			if firstResponse.Code != http.StatusNoContent {
				t.Fatalf("first request status = %d; want 204", firstResponse.Code)
			}

			next := httptest.NewRequest(http.MethodPost, "/api/v1/register", nil)
			next.RemoteAddr = test.nextAddr
			for name, value := range test.headers {
				next.Header.Set(name, value)
			}

			response := httptest.NewRecorder()
			limiter.ServeHTTP(response, next)
			if response.Code != test.status {
				t.Fatalf("next request status = %d; want %d", response.Code, test.status)
			}
		})
	}
}

func TestRateLimiterConcurrentRequests(t *testing.T) {
	const requests = 100
	const burst = 20
	now := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 10, burst)
	limiter.now = func() time.Time { return now }
	start := make(chan struct{})
	results := make(chan int, requests)
	for range requests {
		go func() {
			<-start
			request := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
			request.RemoteAddr = "192.0.2.1:1000"
			response := httptest.NewRecorder()
			limiter.ServeHTTP(response, request)
			results <- response.Code
		}()
	}

	close(start)

	allowed := 0
	for range requests {
		switch status := <-results; status {
		case http.StatusNoContent:
			allowed++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected concurrent response: %d", status)
		}
	}

	if allowed != burst {
		t.Fatalf("allowed requests = %d; want %d", allowed, burst)
	}
}
