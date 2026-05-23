// Package middleware — secheaders.go adds security-related HTTP headers.
//
// Security fix: OWASP A05:2021 — Security Misconfiguration.
// Without these headers, browsers may interpret uploaded files as HTML/JS
// (XSS via content sniffing), embed the API in iframes (clickjacking),
// or cache sensitive responses in shared proxies.
package middleware

import "net/http"

// SecurityHeaders returns a middleware that sets security-related HTTP headers
// on every response.
//
// Headers set:
//   - X-Content-Type-Options: nosniff — prevents browsers from MIME-sniffing
//     uploaded files as HTML/JS, mitigating stored XSS attacks.
//   - X-Frame-Options: DENY — prevents clickjacking by blocking iframe embedding.
//   - Cache-Control: no-store — prevents caching of API responses in shared proxies.
//   - Referrer-Policy: strict-origin-when-cross-origin — limits referrer leakage.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
