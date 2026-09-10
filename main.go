package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// version 可通过 ldflags 在构建时注入：go build -ldflags "-X main.version=v0.2.56"
var version = "v0.2.130"

var verboseLevel int // 0=关闭 1=-v 2=-vv（仅快速模式生效）

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}

	// 顶层 --version / -V 标志
	if args[0] == "--version" || args[0] == "-V" {
		fmt.Println("agent-proxy", version)
		os.Exit(0)
	}

	command := args[0]
	switch command {
	case "run":
		runServer(args[1:])
	case "add":
		runDBAdd(args[1:])
	case "detect":
		runDBAdd(args[1:])
	case "query", "q", "list", "l":
		if err := cmd.RunDBQuery(nil); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "check", "c":
		var checkID *int
		if len(args) > 1 {
			if args[1] == "all" {
				zero := 0
				checkID = &zero
			} else {
				id, _ := strconv.Atoi(args[1])
				checkID = &id
			}
		}
		if err := cmd.RunDBCheck(checkID); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "update", "u":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy update <id|all>\n")
			os.Exit(1)
		}
		var updateID int
		if args[1] == "all" {
			updateID = 0
		} else {
			updateID, _ = strconv.Atoi(args[1])
		}
		if err := cmd.RunDBUpdate(updateID); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy rm <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[1])
		if err := cmd.RunDBRm(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "find", "f":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db find <关键词>\n")
			os.Exit(1)
		}
		if err := cmd.RunDBFind(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "db":
		runDBCommand(args[1:])
	case "version":
		fmt.Println("agent-proxy", version)
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
	command := "query"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "add":
		runDBAdd(args)
	case "detect":
		runDBAdd(args)
	case "query", "q", "list", "l":
		if len(args) == 0 {
			if err := cmd.RunDBQuery(nil); err != nil {
				fmt.Fprintf(os.Stderr, "❌ %v\n", err)
				os.Exit(1)
			}
		} else {
			id, _ := strconv.Atoi(args[0])
			if err := cmd.RunDBQuery(&id); err != nil {
				fmt.Fprintf(os.Stderr, "❌ %v\n", err)
				os.Exit(1)
			}
		}
	case "check", "c":
		var checkID *int
		if len(args) > 0 {
			if args[0] == "all" {
				zero := 0
				checkID = &zero
			} else {
				id, _ := strconv.Atoi(args[0])
				checkID = &id
			}
		}
		if err := cmd.RunDBCheck(checkID); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "update", "u":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db update <id|all>\n")
			os.Exit(1)
		}
		var updateID int
		if args[0] == "all" {
			updateID = 0
		} else {
			updateID, _ = strconv.Atoi(args[0])
		}
		if err := cmd.RunDBUpdate(updateID); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "rm":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db rm <id>\n")
			os.Exit(1)
		}
		id, _ := strconv.Atoi(args[0])
		if err := cmd.RunDBRm(id); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	case "find", "f":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "用法: agent-proxy db find <关键词>\n")
			os.Exit(1)
		}
		if err := cmd.RunDBFind(args[0]); err != nil {
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

// runDBAdd 添加代理配置（自动嗅探所有支持的协议）
func runDBAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "Provider 名称")
	url := fs.String("url", "", "Provider URL (必选)")
	key := fs.String("key", "", "API Key (必选)")
	fs.Parse(args)

	if *url == "" || *key == "" {
		fmt.Fprintf(os.Stderr, "❌ --url 和 --key 为必填参数\n")
		fmt.Fprintf(os.Stderr, "用法: agent-proxy db add --url <url> --key <key> [--name <n>]\n")
		os.Exit(1)
	}
	if *name == "" {
		*name = *url
	}
	if err := cmd.RunDBAdd(*name, *url, *key); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

// validRunFlags run 命令允许的参数
var validRunFlags = map[string]bool{
	"--host": true, "--port": true, "--mode": true,
	"--db": true, "--conf": true, "--key": true, "--nokey": true,
	"--aliases": true, "--timeout": true,
	"--read-timeout": true, "--write-timeout": true,
	"-v": true, "-vv": true,
}

// validRunMode 允许的 mode 值
var validRunMode = map[string]bool{"simple": true, "complex": true}

// runServer 解析启动参数并启动服务
func runServer(args []string) {
	host := "127.0.0.1"
	port := 8080
	mode := ""
	modeGiven := false
	dbID := -1
	conf := ""
	clientKey := ""
	keyGiven := false
	noClientKey := false
	aliasPath := ""
	timeout := 300
	readTimeout := 120
	writeTimeout := 600
	// 这两个标志位区分「命令行显式给了」和「用默认值」：
	// 配置文件里的 server.read_timeout/write_timeout 只在命令行没给时生效，
	// 否则命令行会被配置悄悄覆盖。默认值与 config.DefaultConfig 一致，
	// 所以两边不冲突时结果是同一个数。
	readTimeoutSet := false
	writeTimeoutSet := false

	for i := 0; i < len(args); i++ {
		flag := args[i]
		if _, ok := validRunFlags[flag]; !ok {
			fmt.Fprintf(os.Stderr, "❌ 未知参数: %s\n", flag)
			fmt.Fprintf(os.Stderr, "用法: agent-proxy run [--mode <simple|complex>] [--db <id>]\n")
			fmt.Fprintf(os.Stderr, "      [--host <h>] [--port <p>] [--conf <f>]\n")
			fmt.Fprintf(os.Stderr, "      [--key <k> | --nokey] [--aliases <f>] [--timeout <seconds>]\n")
			fmt.Fprintf(os.Stderr, "      [--read-timeout <seconds>] [--write-timeout <seconds>]\n")
			os.Exit(1)
		}
		switch flag {
		case "--host":
			i++
			if i < len(args) {
				host = args[i]
			}
		case "--port":
			i++
			if i < len(args) {
				port, _ = strconv.Atoi(args[i])
			}
		case "--mode":
			i++
			if i < len(args) {
				mode = args[i]
				modeGiven = true
			}
		case "--db":
			i++
			if i < len(args) {
				dbID, _ = strconv.Atoi(args[i])
			}
		case "--conf":
			i++
			if i < len(args) {
				conf = args[i]
			}
		case "--aliases":
			i++
			if i < len(args) {
				aliasPath = args[i]
			}
		case "--key":
			i++
			if i < len(args) {
				clientKey = args[i]
				keyGiven = true
			}
		case "--nokey":
			noClientKey = true
		case "--timeout":
			i++
			if i < len(args) {
				timeout, _ = strconv.Atoi(args[i])
			}
		case "--read-timeout":
			i++
			if i < len(args) {
				readTimeout, _ = strconv.Atoi(args[i])
				readTimeoutSet = true
			}
		case "--write-timeout":
			i++
			if i < len(args) {
				writeTimeout, _ = strconv.Atoi(args[i])
				writeTimeoutSet = true
			}
		case "-v":
			verboseLevel = 1
		case "-vv":
			verboseLevel = 2
		}
	}

	if modeGiven && !validRunMode[mode] {
		fmt.Fprintf(os.Stderr, "❌ --mode 无效: %q（仅支持 simple 或 complex）\n", mode)
		fmt.Fprintf(os.Stderr, "用法: agent-proxy run [--mode <simple|complex>] [--db <id>] [--host <h>] [--port <p>] [--conf <f>] [--key <k> | --nokey]\n")
		os.Exit(1)
	}

	if keyGiven && !modeGiven && dbID <= 0 {
		fmt.Fprintf(os.Stderr, "⚠️  警告: --key 仅在快速模式（--mode simple 或 --db <id>）下有效，当前为复杂模式，将被忽略\n")
	}

	quickMode := mode == "simple" || dbID > 0
	if mode == "complex" {
		quickMode = false
	}

	if quickMode && dbID <= 0 {
		fmt.Fprintf(os.Stderr, "❌ 快速模式需要指定 --db <id>\n")
		os.Exit(1)
	}

	// 快速模式下解析认证密钥：--key <k> 固定密钥 / 默认随机生成 / --nokey 无认证
	quickClientKey := ""
	quickClientKeyEnabled := false
	if quickMode {
		quickClientKeyEnabled = !noClientKey
		if quickClientKeyEnabled {
			if clientKey != "" {
				quickClientKey = clientKey
			} else {
				quickClientKey = generateRandomKey()
			}
		}
	}

	var handler http.Handler
	if quickMode {
		quickHandler, err := startQuickMode(dbID, quickClientKey, quickClientKeyEnabled, aliasPath, timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 启动快速模式失败: %v\n", err)
			os.Exit(1)
		}
		handler = quickHandler
	} else {
		handler = startComplexMode(conf)
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	// 从配置文件取超时（快速模式没有配置文件，conf 为空时 Load 返回默认配置，
	// 而默认值与上面的命令行默认值一致，所以不冲突）。命令行显式给的值优先，
	// 避免「命令行写了、配置文件偷偷盖掉」。
	if !readTimeoutSet || !writeTimeoutSet {
		if cfgFile, err := config.Load(conf); err == nil {
			if !readTimeoutSet && cfgFile.Server.ReadTimeout > 0 {
				readTimeout = cfgFile.Server.ReadTimeout
			}
			if !writeTimeoutSet && cfgFile.Server.WriteTimeout > 0 {
				writeTimeout = cfgFile.Server.WriteTimeout
			}
		}
		// err 忽略：startComplexMode 会重新加载该文件，失败时在那里报错退出
	}
	// ReadTimeout 覆盖「整个请求读取」（头+体），不是只看 header——
	// 上传较慢时大请求体会在这里被掐断，报 read_body i/o timeout。
	// 所以调大前先确认是不是真的在等 body。
	// WriteTimeout 管 SSE 长连接，短了会让长推理的流在 response.completed 前被掐。
	srv := &http.Server{
		Addr:           addr,
		Handler:        handler,
		ReadTimeout:    time.Duration(readTimeout) * time.Second,
		WriteTimeout:   time.Duration(writeTimeout) * time.Second,
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
		fmt.Printf("\n🚀 Agent-Proxy %s (快速模式) running on http://%s:%d\n", version, host, port)
		if quickClientKeyEnabled {
			fmt.Printf("🔑 Proxy Key: %s\n", quickClientKey)
			fmt.Printf("🔐 客户端需使用 Authorization: Bearer %s 连接\n", quickClientKey)
		} else {
			fmt.Printf("🔓 无需认证密钥（--nokey）\n")
		}
		fmt.Printf("📝 Chat Completions: POST http://localhost:%d/v1/chat/completions\n", port)
		fmt.Printf("💬 Anthropic Messages: POST http://localhost:%d/v1/messages\n", port)
		fmt.Printf("🔮 Gemini:            POST http://localhost:%d/v1/models/{model}:generateContent\n", port)
		fmt.Printf("🤖 OpenAI Responses: POST http://localhost:%d/v1/responses\n", port)
		fmt.Printf("🔍 Model list:        GET  http://localhost:%d/v1/models\n", port)
		fmt.Printf("🏥 Health check:      GET  http://localhost:%d/health\n", port)
	} else {
		fmt.Printf("\n🚀 Agent-Proxy %s (复杂模式) running on http://%s:%d\n", version, host, port)
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
func startQuickMode(dbID int, clientKey string, clientKeyEnabled bool, aliasPath string, timeout int) (http.Handler, error) {
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
		return nil, fmt.Errorf("未找到 ID=%d 的代理配置。使用 'agent-proxy db query' 查看现有配置", dbID)
	}

	fmt.Printf("⚡ 快速模式: 使用 DB 记录 #%d\n", dbID)
	fmt.Printf("  Provider: %s (%s)\n", record.Name, record.URL)
	fmt.Printf("  Type:     %s\n", record.ProviderType)

	baseURL := record.URL
	if record.ProviderType == "openai" {
		baseURL = normalizeBaseURL(baseURL)
	}

	quick := server.NewQuickGateway(record.Name, baseURL, record.Key, record.Capabilities(), record.ModelsMap(), record.UpstreamType, record.OpenAIPath, timeout, clientKey, clientKeyEnabled, verboseLevel)
	if aliasPath != "" {
		af, err := db.LoadAliasFile(aliasPath)
		if err != nil {
			return nil, err
		}
		quick.SetAliasFile(af)
		fmt.Printf("  Aliases:  %s\n", aliasPath)
	} else {
		af, loaded := db.LoadAliasFileAuto("", func(msg string) {
			fmt.Fprintf(os.Stderr, "⚠  %s\n", msg)
		})
		if loaded {
			fmt.Printf("  Aliases:  %s\n", af.Path())
		}
		quick.SetAliasFile(af)
	}
	return quick.Routes(), nil
}

// generateRandomKey 生成一个随机的 24 字节 hex 密钥（共 48 字符）
func generateRandomKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "sk-" + time.Now().Format("20060102150405")
	}
	return "sk-" + hex.EncodeToString(b)
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

	return server.NewGateway(cfg, verboseLevel).Routes()
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
  agent-proxy run --db <id>                                              快速模式（默认，随机生成密钥）
  agent-proxy run --db <id> --key <k>                                   快速模式（指定客户端密钥）
  agent-proxy run --db <id> --nokey                                     快速模式（无需客户端密钥）
  agent-proxy run --mode complex                                         复杂模式（默认配置）
  agent-proxy run --mode complex --host <h> --port <p>                   复杂模式（指定监听地址/端口）
  agent-proxy run --mode complex --host <h> --port <p> --conf <f>        复杂模式（配置文件）

  参数说明:
    --db <id>    从数据库选一条记录启动（快速模式）
    --host       监听地址（默认 127.0.0.1，用 0.0.0.0 允许远程访问）
    --port       监听端口（默认 8080）
    --mode       simple / complex（默认由 --db 自动判断）
    --conf       复杂模式配置文件路径
    --key <k>    快速模式客户端密钥（默认随机生成并显示）
    --nokey      快速模式不要求客户端密钥（本地开发用）
    --timeout <seconds>  上游请求超时秒数（默认 300，即 5 分钟）
    --read-timeout <seconds>   入站请求读取超时秒数（默认 120；超时后请求报 read_body i/o timeout）
    --write-timeout <seconds>  响应写出超时秒数（默认 600；SSE 长推理需要更长）
    -v           快速模式请求日志：客户端 IP / 入站协议 / 上游 / token 用量 / 耗时
    -vv          快速模式四向日志：依次显示 [Guest→代理] [代理→LLM] [LLM→代理] [代理→Guest]

数据库命令:
  agent-proxy db add      --url <u> --key <k> [--name <n>]  新增代理
  agent-proxy db rm       <id>                              删除代理
  agent-proxy db query    [id]                              查询代理（无 id 列出全部，alias: list / l / q）
  agent-proxy db find     <关键词>                           搜索代理
  agent-proxy db check                      核对所有代理（重新探测，提示删除无效记录）
  agent-proxy db detect  --url <u> --key <k> [--name <n>]  新增代理（兼容 alias → add）

其他命令:
  agent-proxy version          查看版本号
  agent-proxy --version, -V    查看版本号

示例:
  # 添加 Sensenova
  agent-proxy db add --url https://token.sensenova.cn/v1 --key sk-xxx --name sensenova

  # 快速启动（本机）
  agent-proxy run --db 1

  # 快速启动（允许远程访问，指定密钥）
  agent-proxy run --db 1 --host 0.0.0.0 --port 8080 --key sk-xxx

  # 快速启动（无需密钥，本地开发）
  agent-proxy run --db 1 --nokey

  # 查询所有代理
  agent-proxy db query

  # 查看 ID=1 详情
  agent-proxy db query 1

  # 核对所有代理有效性
  agent-proxy db check

  # 复杂模式
  agent-proxy run --mode complex --host 0.0.0.0 --port 8080

  # 复杂模式 + 配置文件
  agent-proxy run --mode complex --host 0.0.0.0 --port 8080 --conf config.json
`)
}
