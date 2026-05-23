// Package middleware — ratelimit.go implements per-IP rate limiting.
//
// Security fix: OWASP A04:2021 — Insecure Design.
// Without rate limiting, attackers can perform DoS via upload floods,
// mass tag creation, or future brute-force attacks on authentication.
// This middleware limits requests per client IP using a token bucket.
package middleware

import (
	"net/http"
	"sync"
	"time"
)

// tokenBucket implements a simple token bucket rate limiter.
type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
	mu         sync.Mutex
}

func newTokenBucket(rps float64, burst int) *tokenBucket {
	return &tokenBucket{
		tokens:     float64(burst),
		maxTokens:  float64(burst),
		refillRate: rps,
		lastRefill: time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimiterStore manages per-IP token buckets with automatic cleanup.
type rateLimiterStore struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rps     float64
	burst   int
}

func newRateLimiterStore(rps float64, burst int) *rateLimiterStore {
	s := &rateLimiterStore{
		buckets: make(map[string]*tokenBucket),
		rps:     rps,
		burst:   burst,
	}
	// Periodic cleanup to prevent memory exhaustion from many unique IPs.
	go s.cleanup()
	return s
}

func (s *rateLimiterStore) getBucket(ip string) *tokenBucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	if b, ok := s.buckets[ip]; ok {
		return b
	}
	b := newTokenBucket(s.rps, s.burst)
	s.buckets[ip] = b
	return b
}

func (s *rateLimiterStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for ip, b := range s.buckets {
			b.mu.Lock()
			// Remove buckets idle for more than 10 minutes.
			if time.Since(b.lastRefill) > 10*time.Minute {
				delete(s.buckets, ip)
			}
			b.mu.Unlock()
		}
		s.mu.Unlock()
	}
}

// RateLimit returns a middleware that limits requests per client IP.
//
// Prevents DoS via upload floods, mass resource creation, and
// future brute-force attacks. Returns 429 Too Many Requests when exceeded.
//
// Parameters:
//   - rps: sustained requests per second per IP
//   - burst: maximum burst size (initial token count)
//
// TODO: We could setup ratelimiter values depending on the client/customers
// so that big ones can have priority over super small ones.  It's a business
// decision though.
func RateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	store := newRateLimiterStore(rps, burst)
	metrics := NewMetrics()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr

			bucket := store.getBucket(ip)
			if !bucket.allow() {
				metrics.IncRateLimitRejection()
				w.Header().Set("Retry-After", "1")
				writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
