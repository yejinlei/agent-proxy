package middleware

import (
	"encoding/json"
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

// Auth API Key 认证，支持两种认证方式：
//   - Authorization: Bearer <key>（OpenAI 兼容 / Responses / Gemini 客户端）
//   - x-api-key: <key>           （Anthropic 客户端）
func Auth(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("x-api-key") == apiKey {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+apiKey {
				sendUnauthorizedJSON(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sendUnauthorizedJSON 发送标准 JSON 格式的 401 错误，与网关 sendError 保持一致。
func sendUnauthorizedJSON(w http.ResponseWriter) {
	errResp := map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "invalid_request_error",
			"message": "invalid api key",
			"code":    "401",
		},
	}
	data, err := json.Marshal(errResp)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write(data)
}
