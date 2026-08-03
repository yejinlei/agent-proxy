package middleware

import (
        "net/http"
)

// Logger 请求日志中间件
func Logger() func(http.Handler) http.Handler {
        return func(next http.Handler) http.Handler {
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        next.ServeHTTP(w, r)
                })
        }
}

// Recoverer panic 恢复
func Recoverer(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                // TODO: implement panic recovery
                next.ServeHTTP(w, r)
        })
}

// CORS 跨域支持
func CORS(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Access-Control-Allow-Origin", "*")
                w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
                w.Header().Set("Access-Control-Allow-Headers", "*")
                if r.Method == "OPTIONS" {
                        w.WriteHeader(http.StatusOK)
                        return
                }
                next.ServeHTTP(w, r)
        })
}

// Auth API Key 认证
func Auth(apiKey string) func(http.Handler) http.Handler {
        return func(next http.Handler) http.Handler {
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        auth := r.Header.Get("Authorization")
                        if auth != "Bearer "+apiKey && r.URL.Path != "/health" {
                                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                                return
                        }
                        next.ServeHTTP(w, r)
                })
        }
}
