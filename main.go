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
	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}
	command := args[0]
	switch command {
	case "run":
		runServer(args[1:])
	case "list":
		runDBList()
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
		runDBAdd(args[1:])
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
	case "db":
		runDBCommand(args[1:])
	case "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

// runDBCommand 处理 agent-proxy db <subcommand> [args]
func runDBCommand(args []string) {
	command := "list"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "list":
		if err := cmd.RunDBList(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "show":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db show <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[0])
		if err := cmd.RunDBShow(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "add":
		runDBAdd(args)
	case "rm", "delete":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db rm <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[0])
		if err := cmd.RunDBDelete(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: db %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func runDBList() {
	if err := cmd.RunDBList(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

// runDBAdd 添加代理配置
func runDBAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "Provider 名称")
	url := fs.String("url", "", "Provider URL (必选)")
	key := fs.String("key", "", "API Key (必选)")
	providerType := fs.String("type", "openai", "Provider 类型: openai/anthropic/gemini")
	fs.Parse(args)

	if *url == "" || *key == "" {
		fmt.Fprintf(os.Stderr, "❌ --url 和 --key 为必填参数\n")
		fmt.Fprintf(os.Stderr, "用法: agent-proxy db add --url <url> --key <key> [--name <n>] [--type <t>]\n")
		os.Exit(1)
	}
	if *name == "" {
		*name = *url
	}
	if err := cmd.RunDBAdd(*name, *url, *key, *providerType); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 已保存代理配置: %s (%s)\n", *name, *url)
}

// runServer 解析启动参数并启动服务
func runServer(args []string) {
	host := "127.0.0.1"
	port := 8080
	mode := ""
	dbID := -1
	conf := ""
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--host":
			host = args[i+1]
		case "--port":
			port, _ = strconv.Atoi(args[i+1])
		case "--mode":
			mode = args[i+1]
		case "--db":
			dbID, _ = strconv.Atoi(args[i+1])
		case "--conf":
			conf = args[i+1]
		}
	}

	// --db N        → 快速模式（默认，需指定 --db）
	// --mode complex [--conf] → 复杂模式
	// 无 --db 也无 --mode → 默认复杂模式
	quickMode := mode == "simple" || dbID > 0
	if mode == "complex" {
		quickMode = false
	}

	if quickMode && dbID <= 0 {
		fmt.Fprintf(os.Stderr, "❌ 快速模式需要指定 --db <id>\n")
		os.Exit(1)
	}

	var handler http.Handler
	if quickMode {
		quickHandler, err := startQuickMode(dbID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 启动快速模式失败: %v\n", err)
			os.Exit(1)
		}
		handler = quickHandler
	} else {
		handler = startComplexMode(conf)
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

	if quickMode {
		fmt.Printf("\n🚀 Agent-Proxy (快速模式) running on http://%s:%d\n", host, port)
		fmt.Printf("📝 Chat Completions: POST http://localhost:%d/v1/chat/completions\n", port)
		fmt.Printf("💬 Anthropic Messages: POST http://localhost:%d/v1/messages\n", port)
		fmt.Printf("🔮 Gemini:            POST http://localhost:%d/v1/models/{model}:generateContent\n", port)
		fmt.Printf("🤖 OpenAI Responses: POST http://localhost:%d/v1/responses\n", port)
		fmt.Printf("🔍 Model list:        GET  http://localhost:%d/v1/models\n", port)
		fmt.Printf("🏥 Health check:      GET  http://localhost:%d/health\n", port)
	} else {
		fmt.Printf("\n🚀 Agent-Proxy (复杂模式) running on http://%s:%d\n", host, port)
		fmt.Printf("📊 Web UI: http://localhost:%d/ui\n", port)
		fmt.Printf("📝 Chat Completions: POST http://localhost:%d/v1/chat/completions\n", port)
		fmt.Printf("💬 Anthropic Messages: POST http://localhost:%d/v1/messages\n", port)
		fmt.Printf("🔮 Gemini:            POST http://localhost:%d/v1/models/{model}:generateContent\n", port)
		fmt.Printf("🤖 OpenAI Responses: POST http://localhost:%d/v1/responses\n", port)
		fmt.Printf("🔍 Model list:        GET  http://localhost:%d/v1/models\n", port)
		fmt.Printf("🏥 Health check:      GET  http://localhost:%d/health\n", port)
	}

	if err := srv.ListenAndServe(); err != nil {
		fmt.Printf("❌ Server error: %v\n", err)
		os.Exit(1)
	}
}

// startQuickMode 从 DB 读取一条记录启动快速网关
func startQuickMode(dbID int) (http.Handler, error) {
	store, err := db.New("")
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
		return nil, fmt.Errorf("未找到 ID=%d 的代理配置。使用 'agent-proxy db list' 查看现有配置", dbID)
	}

	fmt.Printf("⚡ 快速模式: 使用 DB 记录 #%d\n", dbID)
	fmt.Printf("  Provider: %s (%s)\n", record.Name, record.URL)
	fmt.Printf("  Type:     %s\n", record.ProviderType)

	baseURL := record.URL
	if record.ProviderType == "openai" {
		baseURL = normalizeBaseURL(baseURL)
	}

	quick := server.NewQuickGateway(record.Name, baseURL, record.Key, record.ProviderType, 60)
	return quick.Routes(), nil
}

// startComplexMode 使用配置启动完整网关
func startComplexMode(confPath string) http.Handler {
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

	if confPath != "" {
		file, err := config.Load(confPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 加载配置文件失败: %v\n", err)
			os.Exit(1)
		}
		cfg = file
	}

	return server.NewGateway(cfg).Routes()
}

// normalizeBaseURL 去掉 URL 末尾的 /v1
func normalizeBaseURL(url string) string {
	if len(url) >= 4 && url[len(url)-3:] == "/v1" {
		return url[:len(url)-3]
	}
	return url
}

func printUsage() {
	fmt.Print(`

🔗 Agent-Proxy — AI 消息协议网关

启动命令:
  agent-proxy run --db <id>                           快速模式（默认，只监听本机）
  agent-proxy run --db <id> --host <h> --port <p>    快速模式（可指定监听地址/端口）
  agent-proxy run --mode complex                       复杂模式（默认配置）
  agent-proxy run --mode complex --host <h> --port <p> --conf <f>  复杂模式（配置文件）

  参数说明:
    --db <id>    从数据库选一条记录启动（快速模式）
    --host       监听地址（默认 127.0.0.1，用 0.0.0.0 允许远程访问）
    --port       监听端口（默认 8080）
    --mode       simple / complex（默认由 --db 自动判断）
    --conf       复杂模式配置文件路径

数据库命令:
  agent-proxy db list                                    列出所有记录
  agent-proxy db show <id>                               显示详情
  agent-proxy db add --url <u> --key <k> [--name <n>]    添加记录
  agent-proxy db rm <id>                                 删除记录

示例:
  # 添加 Sensenova
  agent-proxy db add --url https://token.sensenova.cn/v1 --key sk-xxx --name sensenova

  # 快速启动（本机）
  agent-proxy run --db 1

  # 快速启动（允许远程访问）
  agent-proxy run --db 1 --host 0.0.0.0 --port 8080

  # 复杂模式
  agent-proxy run --mode complex --host 0.0.0.0 --port 8080

  # 复杂模式 + 配置文件
  agent-proxy run --mode complex --host 0.0.0.0 --port 8080 --conf config.json
`)
}
