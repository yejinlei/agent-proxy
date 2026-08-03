package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/agent-proxy/agent-proxy/cmd"
	"github.com/agent-proxy/agent-proxy/internal/config"
	"github.com/agent-proxy/agent-proxy/internal/db"
	"github.com/agent-proxy/agent-proxy/internal/server"
)

func main() {
	args := os.Args[1:]

	// CLI 子命令检测
	cliCommands := map[string]bool{
		"list": true, "show": true, "add": true,
		"rm": true, "delete": true, "--help": true, "-h": true, "db": true,
	}
	if len(args) > 0 && cliCommands[args[0]] {
		runCLI(args)
		return
	}

	// ========== 启动模式解析 ==========
	// 超简易模式: --db N  从 DB 选一条记录
	// 复杂模式: 默认（使用配置）

	dbID := -1
	dbPath := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--db" {
			dbID, _ = strconv.Atoi(args[i+1])
		}
		if args[i] == "--dbpath" {
			dbPath = args[i+1]
		}
	}

	host := "0.0.0.0"
	port := 8080
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--host" {
			host = args[i+1]
		}
		if args[i] == "--port" {
			port, _ = strconv.Atoi(args[i+1])
		}
	}

	var handler http.Handler

	if dbID > 0 {
		quickHandler, err := startQuickMode(dbID, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 启动快速模式失败: %v\n", err)
			os.Exit(1)
		}
		handler = quickHandler
	} else {
		handler = startComplexMode()
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	srv := &http.Server{
		Addr:           addr,
		Handler:        handler,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\n⚠️  Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if dbID > 0 {
		fmt.Printf("\n🚀 Agent-Proxy (快速模式) running on http://localhost:%d\n", port)
		fmt.Printf("📝 POST http://localhost:%d/v1/chat/completions\n", port)
		fmt.Printf("🏥 http://localhost:%d/health\n", port)
	} else {
		fmt.Printf("\n🚀 Agent-Proxy running on http://localhost:%d\n", port)
		fmt.Printf("📊 Web UI: http://localhost:%d/ui\n", port)
		fmt.Printf("📝 Chat Completions: POST http://localhost:%d/v1/chat/completions\n", port)
	}

	if err := srv.ListenAndServe(); err != nil {
		fmt.Printf("❌ Server error: %v\n", err)
		os.Exit(1)
	}
}

// startQuickMode 从 DB 读取一条记录启动快速网关
func startQuickMode(dbID int, dbPath string) (http.Handler, error) {
	store, err := db.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	if err := store.Init(); err != nil {
		return nil, fmt.Errorf("init db: %w", err)
	}

	record, err := store.GetByID(dbID)
	if err != nil {
		return nil, fmt.Errorf("get record: %w", err)
	}
	if record == nil {
		return nil, fmt.Errorf("未找到 ID=%d 的代理配置。使用 'list' 查看现有配置", dbID)
	}

	fmt.Printf("⚡ 快速模式: 使用 DB 记录 #%d\n", dbID)
	fmt.Printf("  Provider: %s (%s)\n", record.Name, record.URL)
	fmt.Printf("  Type:     %s\n", record.ProviderType)
	if record.ModelsJSON != "" {
		models := record.Models()
		if len(models) > 0 {
			fmt.Printf("  Models:   %d (%s, ...)\n", len(models), models[0])
		} else {
			fmt.Printf("  Models:   (JSON 解析失败)\n")
		}
	}

	// Sensenova 的 URL 默认带了 /v1，OpenAIClient 会追加 /v1/chat/completions
	// 需要去掉 /v1 前缀
	baseURL := record.URL
	if record.ProviderType == "openai" {
		baseURL = normalizeBaseURL(baseURL)
	}

	// Sensenova 兼容模式：URL 是 https://token.sensenova.cn/v1
	// 实际 endpoint 是 https://token.sensenova.cn/v1/chat/completions
	// 所以 baseURL 应该去掉末尾 /v1
	quick := server.NewQuickGateway(record.Name, baseURL, record.Key, record.ProviderType, 60)
	return quick.Routes(), nil
}

// startComplexMode 使用配置启动完整网关
func startComplexMode() http.Handler {
	cfg := config.DefaultConfig()
	cfg.Providers = make(map[string]*config.ProviderConfig)

	cfg.Providers["sensenova"] = &config.ProviderConfig{
		BaseURL:      "https://token.sensenova.cn",
		APIToken:     os.Getenv("AGENT_PROXY_API_KEY"),
		ProviderType: "openai",
		Models: []string{
			"sensenova-6.7-flash-lite",
			"deepseek-v4-flash",
			"glm-5.2",
			"sensenova-u1-fast",
		},
		Weight:     100,
		TimeoutSec: 60,
		RateLimit:  100,
	}

	cfg.Providers["anthropic"] = &config.ProviderConfig{
		BaseURL:      "https://api.anthropic.com",
		APIToken:     os.Getenv("ANTHROPIC_API_KEY"),
		ProviderType: "anthropic",
		APIVersion:   "2023-06-01",
		Models: []string{
			"claude-3-5-sonnet-20241022",
			"claude-3-5-haiku-20241022",
		},
		Weight:     100,
		TimeoutSec: 120,
		RateLimit:  50,
	}

	cfg.ModelRouter.ModelToProvider = map[string]string{
		"sensenova-": "sensenova",
		"deepseek-":  "sensenova",
		"glm-":       "sensenova",
		"claude-":    "anthropic",
		"gemini-":    "gemini",
	}
	cfg.ModelRouter.DefaultProvider = "sensenova"

	return server.NewGateway(cfg).Routes()
}

// normalizeBaseURL 去掉 URL 末尾的 /v1（如果存在），因为 BuildURL 会追加
func normalizeBaseURL(url string) string {
	if len(url) >= 4 && url[len(url)-3:] == "/v1" {
		return url[:len(url)-3]
	}
	return url
}

// runCLI 处理 CLI 子命令
func runCLI(args []string) {
	command := args[0]

	// 兼容 agent-proxy db list/show/rm 风格
	if command == "db" && len(args) > 1 {
		command = args[1]
		args = args[2:]
	}

	switch command {
	case "list", "":
		if err := cmd.RunDBList(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

	case "show":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy show <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[1])
		if err := cmd.RunDBShow(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

	case "add":
		addArgs := flag.NewFlagSet("add", flag.ExitOnError)
		name := addArgs.String("name", "", "Provider 名称")
		url := addArgs.String("url", "", "Provider URL (必选)")
		key := addArgs.String("key", "", "API Key (必选)")
		providerType := addArgs.String("type", "openai", "Provider 类型: openai/anthropic/gemini")
		addArgs.Parse(args[1:])

		if *url == "" || *key == "" {
			fmt.Fprintf(os.Stderr, "❌ --url 和 --key 为必填参数\n")
			fmt.Fprintf(os.Stderr, "用法: agent-proxy add --url <url> --key <key> [--name <n>] [--type <t>]\n")
			os.Exit(1)
		}
		if *name == "" {
			*name = *url
		}
		if err := cmd.RunDBAdd(*name, *url, *key, *providerType); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy rm <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[1])
		if err := cmd.RunDBDelete(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

	case "--help", "-h":
		printUsage()

	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`
🔗 Agent-Proxy — AI 消息协议网关

用法:
  agent-proxy                           # 复杂模式启动（默认配置）
  agent-proxy --db <id>                 # 快速模式：从 DB 选一条记录
  agent-proxy --host <h> --port <p>     # 指定监听地址
  agent-proxy --dbpath <path>           # 指定数据库路径

CLI 命令（DB 管理）:
  agent-proxy list                      # 列出所有代理配置
  agent-proxy show <id>                 # 显示指定代理详情
  agent-proxy add --url <url> --key <key> [--name <n>] [--type <t>]
                                        # 添加代理配置（自动嗅探模型列表）
  agent-proxy rm <id>                   # 删除代理配置

示例:
  # 添加 Sensenova 代理
  agent-proxy add --url https://token.sensenova.cn/v1 \
                  --key sk-xxx --name sensenova

  # 查看所有记录
  agent-proxy list

  # 快速模式启动（使用 DB 第 1 条记录）
  agent-proxy --db 1

  # 复杂模式启动（带自定义端口）
  agent-proxy --host 0.0.0.0 --port 9090
`)
}
