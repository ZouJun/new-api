// Package main 是 New API 项目的主入口
// New API 是一个大模型网关系统，用于统一管理和代理多种 AI 模型的 API 调用
package main

import (
	"bytes"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	_ "net/http/pprof"
)

// buildFS 嵌入前端构建产物目录，用于静态文件服务
//
//go:embed web/d
//go:embed web/dist
var buildFS embed.FS

入口 HTML 文件，用于动态注入分析脚本
//
//go:embed we
// indexPage 嵌入前端入口 HTML 文件，用于动态注入分析脚本
//go:embed web/dist/index.html
var indexPage []byte

// main 是应用程序的主入口函数
// 负责初始化各种资源、启动后台任务、配置路由并启动 HTTP 服务器
func main() {
	// 记录启动时间，用于计算启动耗时
	startTime := time.Now()

	// 初始化所有必需的资源（数据库、Redis、配置等）
	err := InitResources()
	if err != nil {
		common.FatalLog("failed to initialize resources: " + err.Error())
		return
	}

	// 打印启动日志
	common.SysLog("New API " + common.Version + " started")
	// 设置 Gin 框架运行模式（debug 或 release）
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	if common.DebugEnabled {
		common.SysLog("running in debug mode")
	}

	// 确保程序退出时关闭数据库连接
	defer func() {
		err := model.CloseDB()
		if err != nil {
			common.FatalLog("failed to close database: " + err.Error())
		}
	}()

	// 如果启用了 Redis，则自动启用内存缓存（兼容旧版本）
	if common.RedisEnabled {
		// for compatibility with old versions
		common.MemoryCacheEnabled = true
	}
	// 初始化内存缓存系统，用于加速渠道信息查询
	if common.MemoryCacheEnabled {
		common.SysLog("memory cache enabled")
		common.SysLog(fmt.Sprintf("sync frequency: %d seconds", common.SyncFrequency))

		// 初始化渠道缓存，添加 panic 恢复和重试机制
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v, retrying once", r))
					// 发生 panic 时尝试修复并重试一次
					_, _, fixErr := model.FixAbility()
					if fixErr != nil {
						common.FatalLog(fmt.Sprintf("InitChannelCache failed: %s", fixErr.Error()))
					}
				}
			}()
			model.InitChannelCache()
		}()

		// 启动后台协程，定期同步渠道缓存
		go model.SyncChannelCache(common.SyncFrequency)
	}

	// 启动后台协程：热更新系统配置选项
	go model.SyncOptions(common.SyncFrequency)

	// 启动后台协程：定期更新配额数据统计（数据看板）
	go model.UpdateQuotaData()

	// 如果设置了渠道更新频率，启动自动更新渠道任务
	if os.Getenv("CHANNEL_UPDATE_FREQUENCY") != "" {
		frequency, err := strconv.Atoi(os.Getenv("CHANNEL_UPDATE_FREQUENCY"))
		if err != nil {
			common.FatalLog("failed to parse CHANNEL_UPDATE_FREQUENCY: " + err.Error())
		}
		go controller.AutomaticallyUpdateChannels(frequency)
	}

	// 启动后台协程：自动测试渠道可用性
	go controller.AutomaticallyTestChannels()

	// 如果是主节点且启用了任务更新，启动批量任务更新协程
	if common.IsMasterNode && constant.UpdateTask {
		// 批量更新 Midjourney 任务状态
		gopool.Go(func() {
			controller.UpdateMidjourneyTaskBulk()
		})
		// 批量更新通用任务状态
		gopool.Go(func() {
			controller.UpdateTaskBulk()
		})
	}
	// 如果启用了批量更新模式，初始化批量更新器
	if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
		common.BatchUpdateEnabled = true
		common.SysLog("batch update enabled with interval " + strconv.Itoa(common.BatchUpdateInterval) + "s")
		model.InitBatchUpdater()
	}

	// 如果启用了性能分析，启动 pprof 服务器和监控
	if os.Getenv("ENABLE_PPROF") == "true" {
		// 在 8005 端口启动 pprof HTTP 服务器
		gopool.Go(func() {
			log.Println(http.ListenAndServe("0.0.0.0:8005", nil))
		})
		// 启动系统监控
		go common.Monitor()
		common.SysLog("pprof enabled")
	}

	// 初始化 Gin HTTP 服务器
	server := gin.New()
	// 添加自定义 panic 恢复中间件，捕获运行时错误并返回友好的错误信息
	server.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		common.SysLog(fmt.Sprintf("panic detected: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Panic detected, error: %v. Please submit a issue here: https://github.com/Calcium-Ion/new-api", err),
				"type":    "new_api_panic",
			},
		})
	}))
	// 注意：不能使用 gzip 中间件，会导致 SSE（Server-Sent Events）无法正常工作
	//server.Use(gzip.Gzip(gzip.DefaultCompression))
	// 添加请求 ID 中间件，为每个请求生成唯一标识
	server.Use(middleware.RequestId())
	// 设置请求日志记录器
	middleware.SetUpLogger(server)
	// 初始化 Session 存储，使用 Cookie 方式
		MaxAge:   2592000,                 // 30 天有效期
		HttpOnly: true,                    // 防止 XSS 攻击
		Secure:   false,                   // 是否仅通过 HTTPS 传输
		MaxAge:   2592000, // 30 天有效期
		HttpOnly: true,    // 防止 XSS 攻击
		Secure:   false,   // 是否仅通过 HTTPS 传输
		SameSite: http.SameSiteStrictMode, // CSRF 防护
	})
	server.Use(sessions.Sessions("session", store))

	// 向前端页面注入 Umami 分析脚本
	InjectUmamiAnalytics()
	// 向前端页面注入 Google Analytics 脚本
	InjectGoogleAnalytics()

	// 设置所有路由（包括 API 路由和前端静态文件路由）
	router.SetRouter(server, buildFS, indexPage)
	// 获取服务器监听端口，优先使用环境变量
	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}

	// 打印启动成功日志，包含启动耗时和监听端口
	common.LogStartupSuccess(startTime, port)

	// 启动 HTTP 服务器，阻塞直到服务器停止
	err = server.Run(":" + port)
	if err != nil {
		common.FatalLog("failed to start HTTP server: " + err.Error())
	}
}

// InjectUmamiAnalytics 将 Umami 分析脚本注入到前端 HTML 页面中
// Umami 是一个开源的网站分析工具
func InjectUmamiAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("UMAMI_WEBSITE_ID") != "" {
		umamiSiteID := os.Getenv("UMAMI_WEBSITE_ID")
		umamiScriptURL := os.Getenv("UMAMI_SCRIPT_URL")
		if umamiScriptURL == "" {
			umamiScriptURL = "https://analytics.umami.is/script.js"
		}
		analyticsInjectBuilder.WriteString("<script defer src=\"")
		analyticsInjectBuilder.WriteString(umamiScriptURL)
		analyticsInjectBuilder.WriteString("\" data-website-id=\"")
		analyticsInjectBuilder.WriteString(umamiSiteID)
		analyticsInjectBuilder.WriteString("\"></script>")
	}
	analyticsInject := analyticsInjectBuilder.String()
	indexPage = bytes.ReplaceAll(indexPage, []byte("<!--umami-->\n"), []byte(analyticsInject))
}

// InjectGoogleAnalytics 将 Google Analytics 4 (gtag.js) 脚本注入到前端 HTML 页面中
// 用于网站访问数据统计和分析
func InjectGoogleAnalytics() {
	analyticsInjectBuilder := &strings.Builder{}
	if os.Getenv("GOOGLE_ANALYTICS_ID") != "" {
		gaID := os.Getenv("GOOGLE_ANALYTICS_ID")
		// Google Analytics 4 (gtag.js)
		analyticsInjectBuilder.WriteString("<script async src=\"https://www.googletagmanager.com/gtag/js?id=")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("\"></script>")
		analyticsInjectBuilder.WriteString("<script>")
		analyticsInjectBuilder.WriteString("window.dataLayer = window.dataLayer || [];")
		analyticsInjectBuilder.WriteString("function gtag(){dataLayer.push(arguments);}")
		analyticsInjectBuilder.WriteString("gtag('js', new Date());")
		analyticsInjectBuilder.WriteString("gtag('config', '")
		analyticsInjectBuilder.WriteString(gaID)
		analyticsInjectBuilder.WriteString("');")
		analyticsInjectBuilder.WriteString("</script>")
	}
	analyticsInject := analyticsInjectBuilder.String()
	indexPage = bytes.ReplaceAll(indexPage, []byte("<!--Google Analytics-->\n"), []byte(analyticsInject))
}

// InitResources 初始化应用程序所需的所有资源
// 包括：环境变量、日志系统、数据库连接、Redis连接、配置项等
// 返回值：如果初始化失败则返回错误
func InitResources() error {
	// 加载 .env 文件中的环境变量（可选）
	err := godotenv.Load(".env")
	if err != nil {
		if common.DebugEnabled {
			common.SysLog("No .env file found, using default environment variables. If needed, please create a .env file and set the relevant variables.")
		}
	}

	// 初始化环境变量配置
	common.InitEnv()

	// 设置日志系统
	logger.SetupLogger()

	// 初始化模型计费比率设置
	ratio_setting.InitRatioSettings()

	// 初始化 HTTP 客户端（用于代理请求）
	service.InitHttpClient()

	// 初始化 Token 编码器（用于计算各种模型的 token 数量）
	service.InitTokenEncoders()

	// 初始化主数据库（SQLite/MySQL/PostgreSQL）
	err = model.InitDB()
	if err != nil {
		common.FatalLog("failed to initialize database: " + err.Error())
		return err
	}

	// 检查系统初始化状态
	model.CheckSetup()

	// 初始化系统配置选项映射（必须在 InitDB 之后）
	model.InitOptionMap()

	// 初始化模型定价信息
	model.GetPricing()

	// 初始化日志数据库（可能与主数据库分离）
	err = model.InitLogDB()
	if err != nil {
		return err
	}

	// 初始化 Redis 客户端（用于缓存和分布式锁）
	err = common.InitRedisClient()
	if err != nil {
		return err
	}
	return nil
}
