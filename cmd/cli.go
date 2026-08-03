package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-proxy/agent-proxy/internal/db"
)

// RunDBList 列出所有代理
func RunDBList() error {
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
		fmt.Println("数据库为空。使用 'db add' 添加代理配置。")
		return nil
	}

	fmt.Printf("\n📋 已保存的代理配置 (%d 条):\n", len(records))
	fmt.Println(strings.Repeat("─", 90))
	fmt.Printf("  %-4s  %-16s  %-42s  %-12s  %-24s  %s\n", "ID", "Name", "URL", "Provider", "OpenAI/Anth", "时间")
	fmt.Println(strings.Repeat("─", 90))

	for _, r := range records {
		caps := fmt.Sprintf("%v/%v", r.OpenAICap, r.AnthropicCap)
		fmt.Printf("  %-4d  %-16s  %-42s  %-12s  %-24s  %s\n",
			r.ID, r.Name, r.URL, r.ProviderType, caps, r.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Println()
	return nil
}

// RunDBShow 显示指定代理详情
func RunDBShow(id int) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	if !store.Exists(id) {
		return fmt.Errorf("未找到 ID=%d 的代理配置。使用 'db list' 查看现有配置", id)
	}

	r, err := store.GetByID(id)
	if err != nil {
		return err
	}

	fmt.Printf("\n🔍 代理配置详情:\n")
	fmt.Printf("  ID:           %d\n", r.ID)
	fmt.Printf("  Name:         %s\n", r.Name)
	fmt.Printf("  URL:          %s\n", r.URL)
	fmt.Printf("  Key:          %s\n", maskKey(r.Key))
	fmt.Printf("  Provider:     %s\n", r.ProviderType)
	fmt.Printf("  检测格式:     %s\n", r.DetectedFormat)
	fmt.Printf("  OpenAI:       %v\n", r.OpenAICap)
	fmt.Printf("  Anthropic:    %v\n", r.AnthropicCap)
	fmt.Printf("  权重:         %d\n", r.Weight)
	fmt.Printf("  时间:         %s\n", r.CreatedAt.Format("2006-01-02 15:04:05"))

	models := r.Models()
	if len(models) > 0 {
		fmt.Printf("  模型列表 (%d):\n", len(models))
		for i, m := range models {
			fmt.Printf("    %3d. %s\n", i+1, m)
		}
	}
	fmt.Println()
	return nil
}

// RunDBAdd 添加代理配置
func RunDBAdd(name, url, key, providerType string) error {
	store, err := openDB()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Init(); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	// 简易嗅探（直接 GET models 端点）
	models, err := sniffModels(url, key, providerType)
	if err != nil {
		fmt.Printf("⚠  嗅探模型列表失败（记录仍会保存）: %v\n", err)
	}

	providerType = normalizeProvider(providerType)

	err = store.Add(&db.ProxyRecord{
		Name:           name,
		URL:            url,
		Key:            key,
		ProviderType:   providerType,
		DetectedFormat: "openai-compatible",
		OpenAICap:      providerType == "openai",
		AnthropicCap:   providerType == "anthropic",
		ModelCount:     len(models),
		Weight:         100,
		CreatedAt:      time.Now(),
	})
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}

	fmt.Printf("✅ 已保存代理配置:\n")
	fmt.Printf("  URL:      %s\n", url)
	fmt.Printf("  Provider: %s\n", providerType)
	if len(models) > 0 {
		fmt.Printf("  模型 (%d):\n", len(models))
		for i, m := range models {
			fmt.Printf("    %3d. %s\n", i+1, m)
		}
	}
	fmt.Println()
	return nil
}

// RunDBDelete 删除代理
func RunDBDelete(id int) error {
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

// 嗅探模型列表（简化版，类似 agent-nexus sniff）
func sniffModels(url, key, providerType string) ([]string, error) {
	// 拼接 /v1/models 端点
	modelsURL := url
	if !strings.HasSuffix(modelsURL, "/v1") {
		modelsURL = strings.TrimSuffix(modelsURL, "/") + "/v1"
	}
	modelsURL += "/models"

	// 构建请求
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 解析 OpenAI 格式的 models 响应
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	data, ok := body["data"].([]interface{})
	if !ok {
		return nil, nil
	}

	var models []string
	for _, item := range data {
		if m, ok := item.(map[string]interface{}); ok {
			if id, ok := m["id"].(string); ok {
				models = append(models, id)
			}
		}
	}
	return models, nil
}

func normalizeProvider(p string) string {
	p = strings.ToLower(p)
	switch p {
	case "anthropic":
		return "anthropic"
	case "gemini", "google":
		return "gemini"
	default:
		return "openai"
	}
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

func openDB() (*db.DB, error) {
	return db.New("")
}
