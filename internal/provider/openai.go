package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/protocol/schema"
)

// openaiClient 内部表示，支持可配置端点
type openaiClient struct {
	name     string
	baseURL  string
	endpoint string
	client   *http.Client
}

// OpenAIClient OpenAI Chat Completions 客户端
type OpenAIClient openaiClient

// NewOpenAIClient OpenAI 兼容 API 客户端（Chat Completions 端点）
func NewOpenAIClient(name, baseURL string, timeout int) *OpenAIClient {
	return &OpenAIClient{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		endpoint: "/v1/chat/completions",
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// NewResponsesClient OpenAI Responses API 客户端（/v1/responses 端点）
// 与 OpenAIClient 共享所有传输和认证逻辑，仅端点不同
func NewResponsesClient(name, baseURL string, timeout int) *OpenAIClient {
	return &OpenAIClient{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		endpoint: "/v1/responses",
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *OpenAIClient) Name() string    { return c.name }
func (c *OpenAIClient) BaseURL() string { return c.baseURL }

func (c *OpenAIClient) DefaultHeaders(info *schema.ProviderInfo) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+info.APIToken)
	headers.Set("Accept", "application/json")
	return headers
}

func (c *OpenAIClient) BuildURL(info *schema.ProviderInfo, model string, stream bool) string {
	return c.baseURL + c.endpoint
}

func (c *OpenAIClient) Endpoint(model string, stream bool) (method string, url string, err error) {
	return "POST", c.BuildURL(&schema.ProviderInfo{}, model, stream), nil
}

func (c *OpenAIClient) Call(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (body json.RawMessage, headers http.Header, err error) {
	url := c.BuildURL(info, info.Name, false)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	httpReq.Header = headers

	log.Printf("[provider] POST %s body_len=%d content_length=%d", url, len(req), httpReq.ContentLength)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	// 保存响应头（下游原样返回）
	respHeaders := http.Header{}
	for k, v := range resp.Header {
		respHeaders[k] = v
	}

	if resp.StatusCode >= 400 {
		return respBody, respHeaders, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, respHeaders, nil
}

func (c *OpenAIClient) CallStream(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (lines <-chan json.RawMessage, headers http.Header, err error) {
	linesCh := make(chan json.RawMessage, 50)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BuildURL(info, info.Name, true), bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Real-IP", "") // 代理头
	httpReq.Header = headers

	go func() {
		defer close(linesCh)
		resp, err := c.client.Do(httpReq)
		if err != nil {
			linesCh <- json.RawMessage(`{"error":{"message":"connection failed: ` + err.Error() + `"}}`)
			return
		}
		defer resp.Body.Close()

		// 保存响应头
		respHeaders := http.Header{}
		for k, v := range resp.Header {
			respHeaders[k] = v
		}
		// 发送第一个事件带响应头信息（用于下游 header 透传）
		meta, _ := json.Marshal(map[string]interface{}{
			"_type":    "headers",
			"_status":  resp.StatusCode,
			"_headers": respHeaders,
		})
		linesCh <- meta

		// 上游返回错误状态码时，不解析为 SSE，直接转为一条错误事件并关闭 channel
		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			errSSE, _ := json.Marshal(map[string]any{
				"_type":   "error",
				"_status": resp.StatusCode,
				"data":    string(errBody),
			})
			linesCh <- errSSE
			return
		}

		reader := lineReader(ctx, resp.Body)

		for {
			select {
			case <-ctx.Done():
				return
			default:
				line, err := reader()
				if err != nil {
					if err != io.EOF {
						linesCh <- json.RawMessage(`{"error":{"message":"` + err.Error() + `"}}`)
					}
					return
				}

				if line == nil {
					continue
				}

				trimmed := strings.TrimSpace(string(line))
				if trimmed == "" || trimmed == "data: [DONE]" || strings.HasPrefix(trimmed, ":") {
					continue
				}

				linesCh <- json.RawMessage(trimmed)
			}
		}
	}()

	return linesCh, nil, nil
}

// lineReader 从 io.Reader 中逐行读取，返回 []byte（不含换行符）
func lineReader(ctx context.Context, r io.Reader) func() ([]byte, error) {
	buf := make([]byte, 4096)
	pos := 0

	return func() ([]byte, error) {
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}

			// 扫描换行符
			for i := 0; i < pos; i++ {
				if buf[i] == '\n' {
					data := make([]byte, i)
					copy(data, buf[:i])
					copy(buf, buf[i+1:pos])
					pos -= i + 1
					return data, nil
				}
			}

			n, err := r.Read(buf[pos:])
			if n > 0 {
				pos += n
				continue
			}

			if err != nil {
				if pos > 0 {
					data := make([]byte, pos)
					copy(data, buf[:pos])
					pos = 0
					return data, err
				}
				return nil, err
			}
		}
	}
}

// AnthropicClient Anthropic Messages API 客户端
type AnthropicClient struct {
	name       string
	baseURL    string
	client     *http.Client
	apiKey     string
	apiVersion string
}

func NewAnthropicClient(name, baseURL, apiKey, version string, timeout int) *AnthropicClient {
	return &AnthropicClient{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
		apiKey:     apiKey,
		apiVersion: version,
	}
}

func (c *AnthropicClient) Name() string    { return c.name }
func (c *AnthropicClient) BaseURL() string { return c.baseURL }

func (c *AnthropicClient) DefaultHeaders(info *schema.ProviderInfo) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+info.APIToken)
	headers.Set("x-api-key", info.APIToken)
	headers.Set("anthropic-version", c.apiVersion)
	headers.Set("Accept", "application/json")
	return headers
}

func (c *AnthropicClient) BuildURL(info *schema.ProviderInfo, model string, stream bool) string {
	return c.baseURL + "/v1/messages"
}

func (c *AnthropicClient) Endpoint(model string, stream bool) (method string, url string, err error) {
	return "POST", c.BuildURL(&schema.ProviderInfo{}, model, stream), nil
}

func (c *AnthropicClient) Call(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (body json.RawMessage, headers http.Header, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BuildURL(info, "", false), bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	httpReq.Header = headers

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	respHeaders := http.Header{}
	for k, v := range resp.Header {
		respHeaders[k] = v
	}

	if resp.StatusCode >= 400 {
		return respBody, respHeaders, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, respHeaders, nil
}

func (c *AnthropicClient) CallStream(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (lines <-chan json.RawMessage, headers http.Header, err error) {
	linesCh := make(chan json.RawMessage, 50)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BuildURL(info, "", true), bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	httpReq.Header = headers

	go func() {
		defer close(linesCh)
		resp, err := c.client.Do(httpReq)
		if err != nil {
			linesCh <- json.RawMessage(`{"error":{"message":"connection failed: ` + err.Error() + `"}}`)
			return
		}
		defer resp.Body.Close()

		respHeaders := http.Header{}
		for k, v := range resp.Header {
			respHeaders[k] = v
		}
		meta, _ := json.Marshal(map[string]interface{}{
			"_type":    "headers",
			"_status":  resp.StatusCode,
			"_headers": respHeaders,
		})
		linesCh <- meta

		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			errSSE, _ := json.Marshal(map[string]interface{}{
				"_type":   "error",
				"_status": resp.StatusCode,
				"data":    string(errBody),
			})
			linesCh <- errSSE
			return
		}

		reader := lineReader(ctx, resp.Body)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				line, err := reader()
				if err != nil {
					if err != io.EOF {
						linesCh <- json.RawMessage(`{"error":{"message":"` + err.Error() + `"}}`)
					}
					return
				}
				if line == nil {
					continue
				}
				trimmed := strings.TrimSpace(string(line))
				if trimmed == "" {
					continue
				}
				linesCh <- json.RawMessage(trimmed)
			}
		}
	}()

	return linesCh, nil, nil
}

// GeminiClient Gemini GenerateContent API 客户端
type GeminiClient struct {
	name    string
	baseURL string
	client  *http.Client
}

func NewGeminiClient(name, baseURL string, timeout int) *GeminiClient {
	return &GeminiClient{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
	}
}

func (c *GeminiClient) Name() string    { return c.name }
func (c *GeminiClient) BaseURL() string { return c.baseURL }

func (c *GeminiClient) DefaultHeaders(info *schema.ProviderInfo) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+info.APIToken)
	return headers
}

func (c *GeminiClient) BuildURL(info *schema.ProviderInfo, model string, stream bool) string {
	// Gemini 端点格式: /v1/models/{model}:generateContent
	// stream 时加 ?key=xxx
	if stream {
		return c.baseURL + fmt.Sprintf("/v1/models/%s:generateContent?key=%s", model, info.APIToken)
	}
	return c.baseURL + fmt.Sprintf("/v1/models/%s:generateContent", model)
}

func (c *GeminiClient) Endpoint(model string, stream bool) (method string, url string, err error) {
	return "POST", c.BuildURL(&schema.ProviderInfo{}, model, stream), nil
}

func (c *GeminiClient) Call(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (body json.RawMessage, headers http.Header, err error) {
	model := ""
	if info != nil {
		model = info.Name
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BuildURL(info, model, false), bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	httpReq.Header = headers

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	respHeaders := http.Header{}
	for k, v := range resp.Header {
		respHeaders[k] = v
	}

	if resp.StatusCode >= 400 {
		return respBody, respHeaders, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, respHeaders, nil
}

func (c *GeminiClient) CallStream(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (lines <-chan json.RawMessage, headers http.Header, err error) {
	linesCh := make(chan json.RawMessage, 50)

	model := ""
	if info != nil {
		model = info.Name
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BuildURL(info, model, true), bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	httpReq.Header = headers

	go func() {
		defer close(linesCh)
		resp, err := c.client.Do(httpReq)
		if err != nil {
			linesCh <- json.RawMessage(`{"error":{"message":"connection failed: ` + err.Error() + `"}}`)
			return
		}
		defer resp.Body.Close()

		respHeaders := http.Header{}
		for k, v := range resp.Header {
			respHeaders[k] = v
		}
		meta, _ := json.Marshal(map[string]interface{}{
			"_type":    "headers",
			"_status":  resp.StatusCode,
			"_headers": respHeaders,
		})
		linesCh <- meta

		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			errSSE, _ := json.Marshal(map[string]interface{}{
				"_type":   "error",
				"_status": resp.StatusCode,
				"data":    string(errBody),
			})
			linesCh <- errSSE
			return
		}

		reader := lineReader(ctx, resp.Body)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				line, err := reader()
				if err != nil {
					if err != io.EOF {
						linesCh <- json.RawMessage(`{"error":{"message":"` + err.Error() + `"}}`)
					}
					return
				}
				if line == nil {
					continue
				}
				trimmed := strings.TrimSpace(string(line))
				if trimmed == "" {
					continue
				}
				// Gemini 流式每行是 data: {...} 格式
				if strings.HasPrefix(trimmed, "data: ") {
					trimmed = trimmed[6:]
				}
				linesCh <- json.RawMessage(trimmed)
			}
		}
	}()

	return linesCh, nil, nil
}
