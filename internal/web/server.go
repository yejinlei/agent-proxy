package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/monitor"
	"github.com/go-chi/chi/v5"
)

//go:embed static
var staticFS embed.FS

// Server 嵌入 Web UI 的 HTTP 服务
type Server struct {
	store  *monitor.Store
	routes chi.Router
}

func NewServer(store *monitor.Store, pathPrefix string) *Server {
	mux := chi.NewRouter()
	s := &Server{
		store:  store,
		routes: mux,
	}
	s.setupRoutes(pathPrefix)
	return s
}

func (s *Server) setupRoutes(_ string) {
	// chi.Mount 会自动 strip prefix，所以这里用相对路径
	s.routes.Get("/", s.handleIndex)
	s.routes.Get("/*", s.handleStatic)

	// API
	s.routes.Get("/api/summary", s.handleSummary)
	s.routes.Get("/api/logs", s.handleLogsSSE)
	s.routes.Get("/api/metrics", s.handleMetricsSSE)
	s.routes.Get("/api/providers", s.handleProviders)
}

func (s *Server) MountPrefix(_ string) {
	// 占位，已废弃
}

func (s *Server) Handle() chi.Router {
	return s.routes
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	f, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tmpl := template.Must(template.New("index").Parse(string(f)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	filePath := chi.URLParam(r, "*")
	assetPath := "static/" + filePath

	f, err := staticFS.Open(assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, _ := f.Stat()
	if info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Content-Type based on extension
	ext := ""
	for i := len(info.Name()) - 1; i >= 0; i-- {
		if info.Name()[i] == '.' {
			ext = info.Name()[i+1:]
			break
		}
	}

	ct := ""
	switch ext {
	case "js":
		ct = "application/javascript; charset=utf-8"
	case "css":
		ct = "text/css; charset=utf-8"
	case "html":
		ct = "text/html; charset=utf-8"
	case "json":
		ct = "application/json"
	}

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))

	// embed.FS's File does not implement io.ReadSeeker, so read and write directly
	data, err := io.ReadAll(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	summary := s.store.GetSummary()
	providers := s.store.GetProviderStatus()

	resp := map[string]interface{}{
		"metrics": map[string]interface{}{
			"qps":                0.0,
			"p99_latency_ms":     0.0,
			"error_rate":         0.0,
			"active_connections": int64(summary["active_conns"].(int64)),
		},
		"providers":      make([]map[string]interface{}, 0),
		"total_requests": int64(summary["total_requests"].(int64)),
		"total_errors":   int64(summary["total_errors"].(int64)),
	}

	totalReq := int64(summary["total_requests"].(int64))
	totalErr := int64(summary["total_errors"].(int64))
	if totalReq > 0 {
		resp["metrics"].(map[string]interface{})["error_rate"] = float64(totalErr) / float64(totalReq) * 100
	}

	for name, p := range providers {
		resp["providers"] = append(resp["providers"].([]map[string]interface{}), map[string]interface{}{
			"name":           name,
			"status":         p.Status,
			"request_count":  p.RequestCount,
			"error_count":    p.ErrorCount,
			"success_count":  p.SuccessCount,
			"avg_latency_ms": p.AvgLatency,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLogsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 发送初始日志
	logs := s.store.GetLogs(0, 100)
	for _, log := range logs {
		data, _ := json.Marshal(map[string]interface{}{
			"time":     log.Time.UnixMilli(),
			"method":   log.Method,
			"path":     log.Path,
			"model":    log.Model,
			"provider": log.Provider,
			"status":   log.StatusCode,
			"latency":  log.LatencyMs,
			"error":    log.ErrorMsg,
		})
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
		flusher.Flush()
	}

	// 心跳 + 轮询新日志
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// 心跳（与代理 SSE 心跳格式保持一致）
			fmt.Fprintf(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			flusher.Flush()

			// 轮询最新日志
			newLogs := s.store.GetLogs(0, 20)
			for _, log := range newLogs {
				data, _ := json.Marshal(map[string]interface{}{
					"time":     log.Time.UnixMilli(),
					"method":   log.Method,
					"path":     log.Path,
					"model":    log.Model,
					"provider": log.Provider,
					"status":   log.StatusCode,
					"latency":  log.LatencyMs,
					"error":    log.ErrorMsg,
				})
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleMetricsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			summary := s.store.GetSummary()
			data, _ := json.Marshal(map[string]interface{}{
				"timestamp": time.Now().Unix(),
				"count":     int64(summary["total_requests"].(int64)),
				"errors":    int64(summary["total_errors"].(int64)),
				"active":    int64(summary["active_conns"].(int64)),
			})

			fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	statuses := s.store.GetProviderStatus()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
}
