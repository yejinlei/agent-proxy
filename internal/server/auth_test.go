package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-proxy/agent-proxy/internal/middleware"
	"github.com/agent-proxy/agent-proxy/internal/protocol/anthropic"
	"github.com/agent-proxy/agent-proxy/internal/protocol/chatcompletion"
	"github.com/agent-proxy/agent-proxy/internal/protocol/gemini"
	"github.com/agent-proxy/agent-proxy/internal/protocol/responses"
	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
	"github.com/agent-proxy/agent-proxy/internal/provider"
	"github.com/agent-proxy/agent-proxy/internal/translator"
)

// --- AuthMiddleware ---

func TestAuthMiddleware_CorrectKey(t *testing.T) {
	h := middleware.Auth("sk-correct")
	seen := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-correct")
	w := httptest.NewRecorder()

	h(next).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !seen {
		t.Error("next handler was not called")
	}
}

func TestAuthMiddleware_WrongKey(t *testing.T) {
	h := middleware.Auth("sk-correct")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called with wrong key")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	w := httptest.NewRecorder()

	h(next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "invalid api key") {
		t.Errorf("expected JSON error containing 'invalid api key', got %q", body)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json content type, got %q", w.Header().Get("Content-Type"))
	}
}

func TestAuthMiddleware_MissingKey(t *testing.T) {
	h := middleware.Auth("sk-correct")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called without key")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	h(next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_HealthEndpointExempted(t *testing.T) {
	h := middleware.Auth("sk-correct")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	h(next).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /health, got %d", w.Code)
	}
}

func TestAuthMiddleware_NonBearerScheme(t *testing.T) {
	h := middleware.Auth("sk-correct")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Token sk-correct")
	w := httptest.NewRecorder()

	h(next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-Bearer scheme, got %d", w.Code)
	}
}

// --- QuickGateway Routes ---

func setupTestQuickGateway(clientKey string, clientKeyEnabled bool) *QuickGateway {
	registry := translator.NewTranslatorRegistry()
	registry.Register(&chatcompletion.ChatCompletionTranslator{})
	registry.Register(anthropic.NewAnthropicTranslator("2023-06-01"))
	registry.Register(gemini.NewGeminiTranslator())
	registry.Register(responses.NewResponsesTranslator())

	// provider.Provider is an interface; use the real OpenAI client (it will
	// fail at call time, but auth tests only need the route to be wired).
	proxyName := "test-proxy"
	proxyBaseURL := "https://test.example.com"
	proxyKey := "pk"

	return &QuickGateway{
		proxyName:          proxyName,
		info:               &schema.ProviderInfo{Name: proxyName, BaseURL: proxyBaseURL, APIToken: proxyKey, Version: "openai"},
		provider:           provider.NewOpenAIClient(proxyName, proxyBaseURL, 5),
		translatorRegistry: registry,
		proxyBaseURL:       proxyBaseURL,
		proxyKey:           proxyKey,
		clientKey:          clientKey,
		clientKeyEnabled:   clientKeyEnabled,
	}
}

func TestQuickGateway_AuthEnabled_CorrectKey(t *testing.T) {
	qg := setupTestQuickGateway("sk-test-key", true)
	mux := qg.Routes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Error("expected /health to be accessible with correct key, got 401")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /health with correct key, got %d", w.Code)
	}
}

func TestQuickGateway_AuthEnabled_WrongKey(t *testing.T) {
	qg := setupTestQuickGateway("sk-test-key", true)
	mux := qg.Routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-wrong")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong key, got %d", w.Code)
	}
}

func TestQuickGateway_AuthEnabled_MissingKey(t *testing.T) {
	qg := setupTestQuickGateway("sk-test-key", true)
	mux := qg.Routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without key, got %d", w.Code)
	}
}

func TestQuickGateway_AuthDisabled_NoKey_Health(t *testing.T) {
	qg := setupTestQuickGateway("", false)
	mux := qg.Routes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 without auth when --nokey, got %d", w.Code)
	}
}

func TestQuickGateway_AuthDisabled_NoKey_ChatCompletions(t *testing.T) {
	qg := setupTestQuickGateway("", false)
	mux := qg.Routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Request must pass the auth layer (no 401). It may 500 from the
	// unreachable test provider, but auth itself has passed.
	if w.Code == http.StatusUnauthorized {
		t.Error("expected auth to pass when --nokey, got 401")
	}
}
