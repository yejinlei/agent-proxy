package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/db"
	"github.com/agent-proxy/agent-proxy/internal/server"
)

// openDB 打开 SQLite 数据库（可通过 dbPathOverride 覆盖路径）
func openDB() (*db.DB, error) {
	path := ""
	if dbPathOverride != nil {
		path = *dbPathOverride
	}
	return db.New(path)
}

// dbPathOverride 用于测试中覆盖默认 DB 路径
var dbPathOverride *string

// RunDBQuery 查询代理（无 id 列出全部，有 id 显示详情）
func RunDBQuery(id *int) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	if id == nil {
		// 列出全部
		records, err := store.List()
		if err != nil {
			return err
		}
		if len(records) == 0 {
			fmt.Println("数据库为空。使用 'agent-proxy db add' 嗅探并添加代理。")
			return nil
		}

		fmt.Printf("\n📋 已保存的代理配置 (%d 条):\n", len(records))
		fmt.Println(strings.Repeat("─", 100))
		fmt.Printf("  %-4s  %-16s  %-42s  %-18s  %-8s  %s\n", "ID", "Name", "URL", "协议", "模型数", "时间")
		fmt.Println(strings.Repeat("─", 100))

		for _, r := range records {
			caps := strings.Join(r.Capabilities(), " / ")
			if caps == "" {
				caps = r.ProviderType
			}
			modelCount := r.TotalModelCount()
			fmt.Printf("  %-4d  %-16s  %-42s  %-18s  %-8d  %s\n",
				r.ID, r.Name, r.URL, caps, modelCount, r.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		fmt.Println()
		return nil
	}

	// 显示详情
	if !store.Exists(*id) {
		return fmt.Errorf("未找到 ID=%d 的代理配置。使用 'agent-proxy db query' 查看现有配置", *id)
	}

	r, err := store.GetByID(*id)
	if err != nil {
		return err
	}

	fmt.Printf("\n🔍 代理配置详情:\n")
	fmt.Printf("  ID:           %d\n", r.ID)
	fmt.Printf("  Name:         %s\n", r.Name)
	fmt.Printf("  URL:          %s\n", r.URL)
	fmt.Printf("  Key:          %s\n", maskKey(r.Key))
	fmt.Printf("  权重:         %d\n", r.Weight)
	fmt.Printf("  时间:         %s\n", r.CreatedAt.Format("2006-01-02 15:04:05"))

	caps := r.Capabilities()
	if len(caps) > 0 {
		fmt.Printf("  协议:         %s\n", strings.Join(caps, " / "))

		hasAnyModel := false
		for _, proto := range caps {
			models := r.ModelsForProtocol(proto)
			if len(models) == 0 {
				continue
			}
			hasAnyModel = true
			fmt.Printf("  %s 模型 (%d):\n", proto, len(models))
			for i, m := range models {
				fmt.Printf("    %3d. %s\n", i+1, m)
			}
		}

		if !hasAnyModel {
			models := r.Models()
			if len(models) > 0 {
				fmt.Printf("  模型列表 (%d):\n", len(models))
				for i, m := range models {
					fmt.Printf("    %3d. %s\n", i+1, m)
				}
			}
		}
	} else {
		fmt.Printf("  Provider:     %s\n", r.ProviderType)
		fmt.Printf("  检测格式:     %s\n", r.DetectedFormat)
		fmt.Printf("  OpenAI:       %v\n", r.OpenAICap)
		fmt.Printf("  Anthropic:    %v\n", r.AnthropicCap)
		models := r.Models()
		if len(models) > 0 {
			fmt.Printf("  模型列表 (%d):\n", len(models))
			for i, m := range models {
				fmt.Printf("    %3d. %s\n", i+1, m)
			}
		}
	}
	fmt.Println()
	return nil
}

// RunDBAdd 添加代理配置（多协议嗅探）
func RunDBAdd(name, url, key string) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	// 检测上游类型（用于请求字段自适应过滤）
	upstreamType := server.DetectUpstreamType(url)

	// 多协议嗅探
	result := sniffAllFn(url, key)

	// 计算总模型数
	totalModels := 0
	for _, ms := range result.ModelsMap {
		totalModels += len(ms)
	}

	// 无模型 → 直接失败，不提示
	if totalModels == 0 {
		fmt.Println("未检测到任何大模型，已取消。")
		return nil
	}

	// 打印嗅探摘要
	fmt.Printf("🔎 探测到 %d 个协议，%d 个模型（上游类型: %s）：\n", len(result.Capabilities), totalModels, upstreamType)
	for _, proto := range result.Capabilities {
		models := result.ModelsMap[proto]
		fmt.Printf("  · %s (%d 个模型)\n", proto, len(models))
	}

	// 提示确认
	fmt.Print("  是否要加到 DB？(yes/no): ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "yes" && answer != "y" {
		fmt.Println("已取消。")
		return nil
	}

	// 写入 DB
	capsJSON, _ := json.Marshal(result.Capabilities)
	modelsMapJSON, _ := json.Marshal(result.ModelsMap)

	// ProviderType 取第一个能力（用于兼容旧字段）
	providerType := "openai"
	if len(result.Capabilities) > 0 {
		providerType = result.Capabilities[0]
	}

	err = store.Add(&db.ProxyRecord{
		Name:             name,
		URL:              url,
		Key:              key,
		ProviderType:     providerType,
		DetectedFormat:   "auto-sniffed",
		OpenAICap:        result.hasProto("openai"),
		AnthropicCap:     result.hasProto("anthropic"),
		ModelCount:       totalModels,
		CapabilitiesJSON: string(capsJSON),
		ModelsMapJSON:    string(modelsMapJSON),
		UpstreamType:     upstreamType,
		Weight:           100,
		CreatedAt:        time.Now(),
	})
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}

	fmt.Printf("✅ 已保存代理配置:\n")
	fmt.Printf("  URL:      %s\n", url)
	fmt.Printf("  协议:     %s\n", strings.Join(result.Capabilities, " / "))
	for _, proto := range result.Capabilities {
		models := result.ModelsMap[proto]
		if len(models) == 0 {
			continue
		}
		fmt.Printf("  %s 模型 (%d):\n", proto, len(models))
		for i, m := range models {
			fmt.Printf("    %3d. %s\n", i+1, m)
		}
	}
	fmt.Println()
	return nil
}

// RunDBRm 删除代理
func RunDBRm(id int) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	if !store.Exists(id) {
		return fmt.Errorf("未找到 ID=%d 的代理配置", id)
	}

	r, _ := store.GetByID(id)
	if r != nil {
		fmt.Printf("⚠  即将删除: ID=%d  %s  %s\n", r.ID, r.Name, r.URL)
		fmt.Print("  确定删除？(yes/no): ")

		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "yes" {
			fmt.Println("已取消。")
			return nil
		}
	}

	if err := store.Delete(id); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	fmt.Printf("✅ 已删除 ID=%d\n", id)
	return nil
}

// RunDBCheck 核对代理配置（重新嗅探，提示更新/删除）
// id nil=全部, id=0=all 显式, id>0=指定 ID
func RunDBCheck(id *int) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	records, err := store.List()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Println("数据库为空，无需核对。")
		return nil
	}

	// 如果指定了 ID，过滤
	if id != nil && *id > 0 {
		targets := make([]db.ProxyRecord, 0)
		for _, r := range records {
			if r.ID == *id {
				targets = append(targets, r)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("未找到 ID=%d 的代理配置", *id)
		}
		records = targets
	}

	fmt.Printf("🔍 正在核对 %d 条代理配置，请稍候...\n\n", len(records))

	type checkResult struct {
		id, prevCount, modelCount int
		name, url, caps, prevCaps string
		valid, changed            bool
	}
	results := make([]checkResult, 0, len(records))
	var invalid, changedIDs []int

	for _, r := range records {
		prevCaps := strings.Join(r.Capabilities(), " / ")
		prevCount := r.TotalModelCount()
		result := sniffAllFn(r.URL, r.Key)
		totalModels := 0
		for _, ms := range result.ModelsMap {
			totalModels += len(ms)
		}
		valid := totalModels > 0
		caps := strings.Join(result.Capabilities, " / ")
		if caps == "" {
			caps = "—"
		}
		cr := checkResult{
			id: r.ID, name: r.Name, url: r.URL, caps: caps,
			modelCount: totalModels, prevCount: prevCount, prevCaps: prevCaps,
			valid: valid, changed: (totalModels != prevCount) || (caps != prevCaps),
		}
		results = append(results, cr)
		if !valid {
			invalid = append(invalid, r.ID)
		} else if cr.changed {
			changedIDs = append(changedIDs, r.ID)
		}
	}

	// 打印汇总表
	fmt.Println(strings.Repeat("─", 120))
	fmt.Printf("  %-4s  %-16s  %-42s  %-18s  %-12s  %s\n", "ID", "Name", "URL", "协议/模型", "变化", "状态")
	fmt.Println(strings.Repeat("─", 120))
	for _, cr := range results {
		status := "✅ 有效"
		if !cr.valid {
			status = "❌ 无效"
		}
		modelStr, changeStr := fmt.Sprintf("%d  (%s)", cr.modelCount, cr.caps), "—"
		if cr.changed {
			changeStr = fmt.Sprintf("%d→%d", cr.prevCount, cr.modelCount)
			modelStr = fmt.Sprintf("%d  (%s→%s)", cr.modelCount, cr.prevCaps, cr.caps)
		}
		fmt.Printf("  %-4d  %-16s  %-42s  %-18s  %-12s  %s\n",
			cr.id, cr.name, cr.url, modelStr, changeStr, status)
	}
	fmt.Println(strings.Repeat("─", 120))

	// 无效记录：提示删除
	if len(invalid) > 0 {
		fmt.Printf("\n⚠️  发现 %d 条无效记录（ID: %v），上游已无法访问\n", len(invalid), invalid)
		fmt.Print("  是否删除无效记录？(yes/no): ")
		answer := readYesNo(os.Stdin)
		if answer {
			for _, id := range invalid {
				if err := store.Delete(id); err != nil {
					fmt.Printf("  ❌ 删除 ID=%d 失败: %v\n", id, err)
				} else {
					fmt.Printf("  ✅ 已删除 ID=%d\n", id)
				}
			}
		}
	}

	// 有变化：提示更新
	if len(changedIDs) > 0 {
		fmt.Printf("\n📡 发现 %d 条记录模型信息有变化（ID: %v），上游模型列表已更新\n", len(changedIDs), changedIDs)
		fmt.Print("  是否更新这些记录？(yes/no): ")
		answer := readYesNo(os.Stdin)
		if answer {
			updated := 0
			for _, cr := range results {
				if !cr.changed || !cr.valid {
					continue
				}
				r, err := store.GetByID(cr.id)
				if err != nil {
					fmt.Printf("  ❌ 读取 ID=%d 失败: %v\n", cr.id, err)
					continue
				}
				sniffResult := sniffAllFn(r.URL, r.Key)
				totalModels := 0
				for _, ms := range sniffResult.ModelsMap {
					totalModels += len(ms)
				}
				capsJSON, _ := json.Marshal(sniffResult.Capabilities)
				modelsMapJSON, _ := json.Marshal(sniffResult.ModelsMap)
				r.ModelCount = totalModels
				r.CapabilitiesJSON = string(capsJSON)
				r.ModelsMapJSON = string(modelsMapJSON)
				r.UpstreamType = server.DetectUpstreamType(r.URL)
				if err := store.Update(r); err != nil {
					fmt.Printf("  ❌ 更新 ID=%d 失败: %v\n", r.ID, err)
					continue
				}
				fmt.Printf("  ✅ 已更新 ID=%d: %s (%d 模型)\n", r.ID, r.Name, totalModels)
				updated++
			}
			fmt.Printf("\n✅ 已更新 %d 条记录。\n", updated)
		}
	}

	if len(invalid) == 0 && len(changedIDs) == 0 {
		fmt.Printf("\n✅ 全部 %d 条记录有效且无变化，无需操作。\n\n", len(records))
	}
	return nil
}

// readYesNo 从标准输入读取 yes/no 确认
func readYesNo(r *os.File) bool {
	reader := bufio.NewReader(r)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "yes" || answer == "y"
}

// RunDBUpdate 更新代理配置的模型/协议信息（重新嗅探后刷新）
// id>0 指定记录, id=0 全部
func RunDBUpdate(id int) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	var targets []db.ProxyRecord
	if id == 0 {
		records, err := store.List()
		if err != nil {
			return err
		}
		targets = records
	} else {
		r, err := store.GetByID(id)
		if err != nil {
			return fmt.Errorf("未找到 ID=%d 的代理配置", id)
		}
		targets = []db.ProxyRecord{*r}
	}

	if len(targets) == 0 {
		fmt.Println("数据库为空，无需更新。")
		return nil
	}

	fmt.Printf("🔄 正在更新 %d 条代理配置的模型信息，请稍候...\n\n", len(targets))

	updated := 0
	invalid := 0
	for i, r := range targets {
		fmt.Printf("  [%d/%d] %s (%s) ... ", i+1, len(targets), r.Name, r.URL)
		result := sniffAllFn(r.URL, r.Key)
		totalModels := 0
		for _, ms := range result.ModelsMap {
			totalModels += len(ms)
		}
		if totalModels == 0 {
			fmt.Println("❌ 无效")
			invalid++
			continue
		}
		capsJSON, _ := json.Marshal(result.Capabilities)
		modelsMapJSON, _ := json.Marshal(result.ModelsMap)
		r.ModelCount = totalModels
		r.CapabilitiesJSON = string(capsJSON)
		r.ModelsMapJSON = string(modelsMapJSON)
		r.UpstreamType = server.DetectUpstreamType(r.URL)
		if err := store.Update(&r); err != nil {
			fmt.Printf("❌ 更新失败: %v\n", err)
			continue
		}
		caps := strings.Join(result.Capabilities, " / ")
		fmt.Printf("✅ %s (%d 模型)\n", caps, totalModels)
		updated++
	}

	fmt.Printf("\n✅ 更新完成：%d 条成功，%d 条无效\n", updated, invalid)
	return nil
}

// SniffResult 多协议嗅探结果
type SniffResult struct {
	Capabilities []string            // ["openai", "anthropic", "gemini", "responses"]
	ModelsMap    map[string][]string // {"openai": ["gpt-4"], "anthropic": ["claude-3"]}
}

func (s *SniffResult) hasProto(p string) bool {
	return slices.Contains(s.Capabilities, p)
}

// sniffAllFn 嗅探函数（可被测试覆盖）
var sniffAllFn = sniffAll

// sniffAll 对上游进行多协议嗅探
func sniffAll(url, key string) *SniffResult {
	result := &SniffResult{
		ModelsMap: make(map[string][]string),
	}
	client := &http.Client{Timeout: 20 * time.Second}

	// 规范化 base URL（去掉 /v1 后缀，便于拼接）
	base := strings.TrimSuffix(url, "/")
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimSuffix(base, "/v1/")

	// ── Step 1: OpenAI ──────────────────────────────────
	openaiURL := base + "/v1/models"
	req, err := http.NewRequest("GET", openaiURL, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			switch resp.StatusCode {
			case 200:
				models := parseOpenAIModels(resp.Body)
				result.ModelsMap["openai"] = models
				result.Capabilities = append(result.Capabilities, "openai")
			case 401:
				result.Capabilities = append(result.Capabilities, "openai")
				result.ModelsMap["openai"] = nil
			}
		}

		// ── Step 1b: OpenAI Responses（独立探测 /v1/responses 端点） ──
		// OpenAI Responses 是 /v1/responses，与 /v1/chat/completions 是不同的端点；
		// 很多 OpenAI 兼容网关（vLLM/SGLang）只支持 chat 不支持 responses，
		// 所以不能用 Step 1 的 200/401 自动推断，必须实际探测端点。
		if len(result.Capabilities) > 0 {
			responsesURL := base + "/v1/responses"
			respBody := map[string]any{
				"model":             "gpt-4o",
				"input":             "probe",
				"max_output_tokens": 1,
				"stream":            false,
			}
			respBodyJSON, _ := json.Marshal(respBody)
			req, err := http.NewRequest("POST", responsesURL, strings.NewReader(string(respBodyJSON)))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("content-type", "application/json")
				req.Header.Set("Accept", "application/json")
				shortClient := &http.Client{Timeout: 10 * time.Second}
				resp, err := shortClient.Do(req)
				if err == nil {
					defer resp.Body.Close()
					if resp.StatusCode < 300 || resp.StatusCode == 401 {
						result.Capabilities = append(result.Capabilities, "responses")
						result.ModelsMap["responses"] = result.ModelsMap["openai"]
					}
				}
			}
		}
	}

	// ── Step 2: Anthropic ────────────────────────────────
	anthropicURL := base + "/v1/messages"
	msgBody := map[string]any{
		"model":      "claude-3-opus-20240229",
		"max_tokens": 1,
		"system":     "probe",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	msgBodyJSON, _ := json.Marshal(msgBody)
	req, err = http.NewRequest("POST", anthropicURL, strings.NewReader(string(msgBodyJSON)))
	if err == nil {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		req.Header.Set("Accept", "application/json")
		shortClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := shortClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode < 300 || resp.StatusCode == 401 {
				var bodyMap map[string]any
				if json.NewDecoder(resp.Body).Decode(&bodyMap) == nil {
					if m, ok := bodyMap["model"].(string); ok {
						result.ModelsMap["anthropic"] = []string{m}
					} else {
						result.ModelsMap["anthropic"] = nil
					}
				} else {
					result.ModelsMap["anthropic"] = nil
				}
				result.Capabilities = append(result.Capabilities, "anthropic")
				// 如果 Anthropic 没有提取到模型名，复用 OpenAI 的模型列表
				if len(result.ModelsMap["anthropic"]) == 0 && len(result.ModelsMap["openai"]) > 0 {
					result.ModelsMap["anthropic"] = result.ModelsMap["openai"]
				}
			}
		}
	}

	// ── Step 3: Gemini ──────────────────────────────────
	geminiURL := base + "/v1/models/gemini-pro:generateContent"
	geminiBody := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"text": "hi"},
				},
			},
		},
	}
	geminiBodyJSON, _ := json.Marshal(geminiBody)
	req, err = http.NewRequest("POST", geminiURL, strings.NewReader(string(geminiBodyJSON)))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("Accept", "application/json")
		shortClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := shortClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode < 300 || resp.StatusCode == 401 {
				result.ModelsMap["gemini"] = nil
				result.Capabilities = append(result.Capabilities, "gemini")
			}
		}
	}

	return result
}

// parseOpenAIModels 从 OpenAI /v1/models 响应解析模型 ID 列表
func parseOpenAIModels(body ioReader) []string {
	var bodyMap map[string]interface{}
	if err := json.NewDecoder(body).Decode(&bodyMap); err != nil {
		return nil
	}
	data, ok := bodyMap["data"].([]interface{})
	if !ok {
		return nil
	}
	var models []string
	for _, item := range data {
		if m, ok := item.(map[string]interface{}); ok {
			if id, ok := m["id"].(string); ok {
				models = append(models, id)
			}
		}
	}
	return models
}

// ioReader 简化为 io.Reader 接口约束
type ioReader interface {
	Read(p []byte) (n int, err error)
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// RunDBFind 按关键词搜索代理（匹配 name / url / capabilities_json / models_map_json）
func RunDBFind(query string) error {
	if query == "" {
		fmt.Fprintf(os.Stderr, "用法: agent-proxy db find <关键词>\n")
		return nil
	}

	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	records, err := store.Search(query)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(records) == 0 {
		fmt.Printf("未找到匹配「%s」的代理配置。\n", query)
		return nil
	}

	fmt.Printf("🔎 搜索「%s」：找到 %d 条匹配记录\n", query, len(records))
	fmt.Println(strings.Repeat("─", 100))
	fmt.Printf("  %-4s  %-16s  %-42s  %-18s  %-8s  %s\n", "ID", "Name", "URL", "协议", "模型数", "时间")
	fmt.Println(strings.Repeat("─", 100))

	for _, r := range records {
		caps := strings.Join(r.Capabilities(), " / ")
		if caps == "" {
			caps = r.ProviderType
		}
		fmt.Printf("  %-4d  %-16s  %-42s  %-18s  %-8d  %s\n",
			r.ID, r.Name, r.URL, caps, r.TotalModelCount(), r.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Println()
	return nil
}
