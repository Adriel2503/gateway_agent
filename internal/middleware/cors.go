package middleware

import (
	"net/http"
	"strings"
)

// CORS sets Access-Control-Allow-* headers. origins is comma-separated (e.g. "*" or "https://app.maravia.pe,https://n8n.maravia.pe").
func CORS(origins string) func(http.Handler) http.Handler {
	allowed := strings.Split(origins, ",")
	for i := range allowed {
		allowed[i] = strings.TrimSpace(allowed[i])
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				for _, o := range allowed {
					if o == "*" || o == origin {
						val := origin
						if o == "*" {
							val = "*"
						}
						w.Header().Set("Access-Control-Allow-Origin", val)
						break
					}
				}
			}
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
