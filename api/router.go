package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// SetupRouter 配置API路由
func SetupRouter() *gin.Engine {
	router := gin.Default()
	router.Use(IPRateLimit())

	// Setup session middleware
	cfg, err := requireConfig()
	if err == nil {
		router.Use(SetupSessionMiddleware(cfg))
	}

	// 健康检查
	router.GET("/health", HealthCheck)

	// Use session-based auth middleware
	authMiddleware := SessionAuthRequired()

	// 飞书免登流程接口
	feishuAuth := router.Group("/api/auth")
	{
		feishuAuth.GET("/session", CheckSession) // 验证 session 有效性
	}

	// Auth config subgroup (完整的认证配置路径)
	feishuAuthConfig := router.Group("/api/auth/config")
	{
		feishuAuthConfig.GET("", GetAuthConfig) // 返回认证配置（appID等）给前端
		feishuAuthConfig.GET("/authorize-url", BuildFeishuAuthorizeURL)
		feishuAuthConfig.POST("/code-login", ExchangeFeishuCode)
		feishuAuthConfig.GET("/jssdk-config", GetJSSDKConfig)
	}

	// CUPS相关接口
	printers := router.Group("/api/printers")
	printers.Use(authMiddleware)
	{
		printers.GET("", ListPrinters)       // 列出所有打印机
		printers.GET("/:id", GetPrinterInfo) // 获取打印机信息
	}

	// 打印任务相关接口
	jobs := router.Group("/api/jobs")
	jobs.Use(authMiddleware)
	{
		jobs.POST("", SubmitPrintJob)                            // 提交打印任务
		jobs.POST("/preview", PreviewConvertedDocument)          // 仅转换并预览文件
		jobs.POST("/preview/feishu", PreviewFeishuDocument)      // 导出飞书文档并预览 PDF
		jobs.POST("/previews", CreateUploadPreviewArtifact)      // 创建可复用的预览文件
		jobs.POST("/previews/feishu", CreateFeishuPreviewArtifact)
		jobs.GET("/previews/:preview_id/file", DownloadPreviewArtifactFile)
		jobs.POST("/previews/:preview_id/renders", CreatePreviewArtifactRender)
		jobs.GET("/previews/:preview_id/renders/:render_id/file", DownloadPreviewArtifactRenderFile)
		jobs.POST("/from-preview", SubmitPrintJobFromPreview)
		jobs.POST("/feishu", SubmitFeishuPrintJob)               // 导出飞书文档并打印
		jobs.GET("/supported-file-types", GetSupportedFileTypes) // 获取当前支持上传的文件类型
		jobs.GET("", ListPrintJobs)                              // 列出所有打印任务
		jobs.GET("/:id", GetJobStatus)                           // 获取任务状态
		jobs.DELETE("/:id", CancelPrintJob)                      // 仅删除多维表任务记录
	}

	// scanservjs 代理接口
	saneAPI := router.Group("/sane-api")
	saneAPI.Use(SaneAPIAuthRequired())
	{
		saneAPI.Any("", SaneAPIProxy())
		saneAPI.Any("/*path", SaneAPIProxy())
	}

	// 手动双面打印相关接口
	manualDuplex := router.Group("/api/manual-duplex-hooks")
	// manualDuplex.Use(authMiddleware)
	{
		manualDuplex.POST("/:token/continue", ContinueManualDuplexPrint) // 继续手动双面打印
		manualDuplex.POST("/:token/extend", ExtendManualDuplexPrint)     // 延长手动双面等待时间
		manualDuplex.POST("/:token/cancel", CancelManualDuplexPrint)     // 取消手动双面打印
	}

	// 飞书 Bot 事件订阅
	bot := router.Group("/api/bot")
	{
		bot.POST("/events", HandleBotEvent)
	}

	// 前端路由：从 public 目录读取静态资源，并为 SPA 路由回退 index.html。
	registerFrontendRoutes(router)

	return router
}

func registerFrontendRoutes(router *gin.Engine) {
	const publicRoot = "public"
	indexPath := filepath.Join(publicRoot, "index.html")

	router.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		method := c.Request.Method

		if strings.HasPrefix(path, "/api/") || path == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		if method != http.MethodGet && method != http.MethodHead {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		cleanPath := filepath.Clean("/" + path)
		if cleanPath == "/" {
			if _, err := os.Stat(indexPath); err == nil {
				c.File(indexPath)
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "frontend index not found"})
			return
		}

		candidate := filepath.Join(publicRoot, strings.TrimPrefix(cleanPath, "/"))
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			c.File(candidate)
			return
		}

		if _, err := os.Stat(indexPath); err == nil {
			c.File(indexPath)
			return
		}

		c.JSON(http.StatusNotFound, gin.H{"error": "frontend index not found"})
	})
}
