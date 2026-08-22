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

// maxRetries 上游请求最大重试次数（0=不重试，默认 2）
const maxRetries = 2

// retryableStatus 判断 HTTP 状态码是否值得重试
func retryableStatus(status int) bool {
	return status == 400 || status == 408 || status == 429 || status >= 500
}

// retryDelay 重试等待时间（指数退避）
func retryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(attempt*attempt) * 100 * time.Millisecond
}

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
// @AI_GUARD: CONNECTION_POOL_CONFIG - 所有 Provider 必须使用相同的连接池配置
// @CONSTRAINT: 4 个 Provider (OpenAI/Anthropic/Gemini/Responses) 必须保持连接池配置一致：
//   MaxIdleConns: 100, MaxIdleConnsPerHost: 100, IdleConnTimeout: 300s
//   - 修改任一 Provider 的连接池配置，必须同步修改其他 3 个
//   - 非流式请求使用独立 http.Client，与 SSE 连接池隔离
// @RELATED: NewAnthropicClient, NewGeminiClient, NewResponsesClient (必须同步)
//
// @AI_GUARD: OPENAI_ENDPOINT_PREFIX - endpointPrefix 必须透传给 BuildURL
//   - 默认 "/v1"（标准 OpenAI / vLLM / SGLang 等）
//   - Google Gemini openai 兼容端点需要 "/v1beta/openai"
func NewOpenAIClient(name, baseURL string, timeout int) *OpenAIClient {
	return newOpenAIClient(name, baseURL, timeout, "/v1")
}

// NewOpenAIClientWithPath 支持指定 endpoint prefix（如 "/v1beta/openai"）
// pathPrefix 为空时回退到默认的 "/v1"
func NewOpenAIClientWithPath(name, baseURL string, timeout int, pathPrefix string) *OpenAIClient {
	if pathPrefix == "" {
		pathPrefix = "/v1"
	}
	return newOpenAIClient(name, baseURL, timeout, pathPrefix)
}

// newOpenAIClient 支持指定 endpointPrefix（如 "/v1beta/openai"）
func newOpenAIClient(name, baseURL string, timeout int, endpointPrefix string) *OpenAIClient {
	endpoint := endpointPrefix + "/chat/completions"
	if endpointPrefix == "" {
		endpoint = "/v1/chat/completions"
	}
	return &OpenAIClient{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		endpoint: endpoint,
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     300 * time.Second,
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
				IdleConnTimeout:     300 * time.Second,
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
	body, headers, err = c.callWithRetry(ctx, req, info, url, false, maxRetries)
	return
}

// callWithRetry 非流式请求重试逻辑（适用于 OpenAI/Responses 客户端）
// 对 400/408/429/5xx 状态码进行最多 maxRetries 次指数退避重试
// @AI_GUARD: RETRYABLE_STATUS - 400/408/429/5xx 值得重试
// @CONSTRAINT: 重试必须保持幂等性——同一请求体重发，不修改请求体
// @RELATED: OpenAIClient.Call, OpenAIClient.CallStream
// @REASON: Sensenova 等上游存在非确定性行为（同请求体有时 200 有时 400），
//   重试可消除服务端负载均衡/限流波动造成的偶发失败
func (c *OpenAIClient) callWithRetry(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo, url string, isStream bool, maxRetries int) (json.RawMessage, http.Header, error) {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req))
		if err != nil {
			return nil, nil, err
		}
		headers := c.DefaultHeaders(info)
		if isStream {
			headers.Set("Accept", "text/event-stream")
			headers.Set("Cache-Control", "no-cache")
			headers.Set("Connection", "keep-alive")
		}
		httpReq.Header = headers

		if attempt > 0 {
			delay := retryDelay(attempt)
			log.Printf("[retry] attempt %d/%d, waiting %v (model=%s)", attempt, maxRetries, delay, extractModel(req))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			log.Printf("[retry] attempt %d/%d POST %s body_len=%d", attempt, maxRetries, url, len(req))
		} else {
			log.Printf("[provider] POST %s body_len=%d content_length=%d", url, len(req), httpReq.ContentLength)
		}

		resp, err := c.client.Do(httpReq)
		if err != nil {
			return nil, nil, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}

		respHeaders := http.Header{}
		for k, v := range resp.Header {
			respHeaders[k] = v
		}

		if resp.StatusCode >= 400 && retryableStatus(resp.StatusCode) && attempt < maxRetries {
			log.Printf("[retry] HTTP %d on attempt %d/%d, body=%s", resp.StatusCode, attempt, maxRetries, string(respBody))
			continue
		}

		if resp.StatusCode >= 400 {
			return respBody, respHeaders, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		return respBody, respHeaders, nil
	}

	return nil, nil, fmt.Errorf("all %d retries exhausted", maxRetries)
}

// extractModel 从请求体中提取 model 名称用于日志
func extractModel(req json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(req, &m); err != nil {
		return "?"
	}
	if model, ok := m["model"].(string); ok {
		return model
	}
	return "?"
}

func (c *OpenAIClient) CallStream(ctx context.Context, req json.RawMessage, info *schema.ProviderInfo) (lines <-chan json.RawMessage, headers http.Header, err error) {
	linesCh := make(chan json.RawMessage, 50)

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")

	go func() {
		defer close(linesCh)
		url := c.BuildURL(info, info.Name, true)

		for attempt := 0; attempt <= maxRetries; attempt++ {
			httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req))
			if err != nil {
				errMsg, _ := json.Marshal(map[string]interface{}{
					"_type":   "error",
					"_status": 502,
					"data":    "connection failed: " + err.Error(),
				})
				linesCh <- errMsg
				return
			}
			headers := c.DefaultHeaders(info)
			headers.Set("Accept", "text/event-stream")
			headers.Set("Cache-Control", "no-cache")
			headers.Set("Connection", "keep-alive")
			httpReq.Header = headers

			if attempt > 0 {
				delay := retryDelay(attempt)
				log.Printf("[retry] attempt %d/%d, waiting %v (model=%s)", attempt, maxRetries, delay, extractModel(req))
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
				log.Printf("[retry] SSE POST %s attempt %d/%d body_len=%d", url, attempt, maxRetries, len(req))
			}

			resp, err := c.client.Do(httpReq)
			if err != nil {
				errMsg, _ := json.Marshal(map[string]interface{}{
					"_type":   "error",
					"_status": 502,
					"data":    "connection failed: " + err.Error(),
				})
				linesCh <- errMsg
				return
			}

			// 保存响应头
			respHeaders := http.Header{}
			for k, v := range resp.Header {
				respHeaders[k] = v
			}

			if resp.StatusCode >= 400 && retryableStatus(resp.StatusCode) && attempt < maxRetries {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				resp.Body.Close()
				log.Printf("[retry] HTTP %d on SSE attempt %d/%d, body=%s", resp.StatusCode, attempt, maxRetries, string(errBody))
				continue
			}

			// 发送响应头元数据（仅最终尝试发送）
			meta, _ := json.Marshal(map[string]interface{}{
				"_type":    "headers",
				"_status":  resp.StatusCode,
				"_headers": respHeaders,
			})
			linesCh <- meta

			if resp.StatusCode >= 400 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				errSSE, _ := json.Marshal(map[string]any{
					"_type":   "error",
					"_status": resp.StatusCode,
					"data":    string(errBody),
				})
				linesCh <- errSSE
				resp.Body.Close()
				return
			}

			reader := lineReader(ctx, resp.Body)
			for {
				select {
				case <-ctx.Done():
					log.Printf("[provider] SSE context cancelled: %v", ctx.Err())
					return
				default:
					line, err := reader()
					if err != nil {
						if err != io.EOF {
							errMsg, _ := json.Marshal(map[string]interface{}{
								"_type":   "error",
								"_status": 502,
								"data":    "stream read failed: " + err.Error(),
							})
							linesCh <- errMsg
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
		}

		// 所有重试次数已用尽
		errMsg, _ := json.Marshal(map[string]interface{}{
			"_type":   "error",
			"_status": 502,
			"data":    fmt.Sprintf("all %d retries exhausted", maxRetries),
		})
		linesCh <- errMsg
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
				log.Printf("[provider] SSE context cancelled: %v", ctx.Err())
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

// sseEventReader 从上游 SSE 流读取完整事件，返回 (event_name, data_json)
// 支持 Anthropic SSE 格式：`:event_name` 注释行 + `data: {json}`
// 也支持标准 SSE 格式：`event: name` + `data: {json}`
func sseEventReader(ctx context.Context, r io.Reader) func() (eventName string, data []byte, err error) {
	lineFn := lineReader(ctx, r)

	return func() (string, []byte, error) {
		var eventName string
		var dataParts [][]byte

		for {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			default:
			}

			line, err := lineFn()
			if err != nil {
				if len(dataParts) > 0 {
					combined := bytes.Join(dataParts, []byte{})
					return eventName, combined, err
				}
				if err == io.EOF {
					return "", nil, err
				}
				return "", nil, err
			}

			trimmed := strings.TrimSpace(string(line))

			// 跳过空行（事件分隔符）
			if trimmed == "" {
				if len(dataParts) > 0 {
					combined := bytes.Join(dataParts, []byte{})
					return eventName, combined, nil
				}
				continue
			}

			// 解析 event: name（标准 SSE）
			if strings.HasPrefix(trimmed, "event: ") {
				eventName = strings.TrimSpace(trimmed[7:])
				continue
			}

			// 解析 data: payload
			if strings.HasPrefix(trimmed, "data: ") {
				payload := trimmed[6:]
				dataParts = append(dataParts, []byte(payload))
				continue
			}

			// Anthropic 风格的注释行：:event_name（如 :message_start）
			if len(trimmed) > 1 && trimmed[0] == ':' && !strings.HasPrefix(trimmed, "://") {
				eventName = trimmed[1:]
				continue
			}

			// 其他情况（原始 data 行，无 data: 前缀），当作 data payload
			dataParts = append(dataParts, []byte(trimmed))
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
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     300 * time.Second,
			},
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
	url := c.BuildURL(info, "", false)
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

	url := c.BuildURL(info, "", true)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	httpReq.Header = headers

	log.Printf("[provider] SSE POST %s body_len=%d content_length=%d", url, len(req), httpReq.ContentLength)

	go func() {
		defer close(linesCh)
		resp, err := c.client.Do(httpReq)
		if err != nil {
			errMsg, _ := json.Marshal(map[string]interface{}{
				"_type":   "error",
				"_status": 502,
				"data":    "connection failed: " + err.Error(),
			})
			linesCh <- errMsg
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
				log.Printf("[provider] SSE context cancelled: %v", ctx.Err())
				return
			default:
				line, err := reader()
				if err != nil {
					if err != io.EOF {
						errMsg, _ := json.Marshal(map[string]interface{}{
							"_type":   "error",
							"_status": 502,
							"data":    "stream read failed: " + err.Error(),
						})
						linesCh <- errMsg
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
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     300 * time.Second,
			},
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
	url := c.BuildURL(info, model, false)
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
	url := c.BuildURL(info, model, true)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}

	headers = c.DefaultHeaders(info)
	headers.Set("Accept", "text/event-stream")
	httpReq.Header = headers

	log.Printf("[provider] SSE POST %s body_len=%d content_length=%d", url, len(req), httpReq.ContentLength)

	go func() {
		defer close(linesCh)
		resp, err := c.client.Do(httpReq)
		if err != nil {
			errMsg, _ := json.Marshal(map[string]interface{}{
				"_type":   "error",
				"_status": 502,
				"data":    "connection failed: " + err.Error(),
			})
			linesCh <- errMsg
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
				log.Printf("[provider] SSE context cancelled: %v", ctx.Err())
				return
			default:
				line, err := reader()
				if err != nil {
					if err != io.EOF {
						errMsg, _ := json.Marshal(map[string]interface{}{
							"_type":   "error",
							"_status": 502,
							"data":    "stream read failed: " + err.Error(),
						})
						linesCh <- errMsg
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
