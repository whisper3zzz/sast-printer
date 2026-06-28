package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"goprint/api/conversion"
	"goprint/config"
	"goprint/cups"

	"github.com/gin-gonic/gin"
)

// extractDuplexEdge 从 duplex 参数提取 edge 类型
func extractDuplexEdge(duplex string) string {
	switch duplex {
	case "long-edge", "two-sided-long-edge":
		return "long-edge"
	case "short-edge", "two-sided-short-edge":
		return "short-edge"
	default:
		return "long-edge" // 默认长边
	}
}

func currentAuthUser(c *gin.Context) (feishuUserInfo, bool) {
	v, ok := c.Get("auth_user")
	if !ok {
		return feishuUserInfo{}, false
	}
	user, ok := v.(feishuUserInfo)
	if !ok {
		return feishuUserInfo{}, false
	}
	if strings.TrimSpace(user.OpenID) == "" {
		return feishuUserInfo{}, false
	}
	return user, true
}

func persistPrintJobToBitable(c *gin.Context, cfg *config.Config, record printJobRecord) {
	store, err := newBitableJobStore(cfg)
	if err != nil {
		log.Printf("[jobs] skip bitable persist: %v", err)
		return
	}

	if err := store.SaveJob(context.Background(), record); err != nil {
		log.Printf("[jobs] bitable persist failed job_id=%s printer=%s err=%v", record.JobID, record.PrinterID, err)
	}
}

func syncManualDuplexExpireAtToBitable(jobID string, expiresAt time.Time) {
	cfg, err := requireConfig()
	if err != nil {
		log.Printf("[manual-duplex] skip bitable expire sync job_id=%s err=%v", jobID, err)
		return
	}

	store, err := newBitableJobStore(cfg)
	if err != nil {
		log.Printf("[manual-duplex] skip bitable expire sync job_id=%s err=%v", jobID, err)
		return
	}

	if err := store.UpdateManualDuplexExpireAt(context.Background(), jobID, expiresAt); err != nil {
		log.Printf("[manual-duplex] failed to sync bitable expire_at job_id=%s expire_at=%s err=%v", jobID, expiresAt.Format(time.RFC3339), err)
	}
}

// HealthCheck 健康检查接口
func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"message": "GoPrint is running.",
	})
}

func applyCopiesMode(sourcePath string, copies int, collate bool) (string, error) {
	if copies <= 1 {
		return sourcePath, nil
	}

	if collate {
		return ApplyCollateCopies(sourcePath, copies)
	}

	return ApplyUncollatedCopies(sourcePath, copies)
}

func parseScalePercent(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 100, nil
	}

	scale, err := strconv.Atoi(raw)
	if err != nil || scale < 10 || scale > 400 {
		return 0, fmt.Errorf("scale must be an integer between 10 and 400")
	}
	return scale, nil
}

const multipartFormOverheadLimit = 1 << 20

func limitRequestBody(c *gin.Context, cfg *config.Config) {
	if cfg == nil || cfg.Printing.MaxUploadBytes <= 0 || c.Request.Body == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cfg.Printing.MaxUploadBytes+multipartFormOverheadLimit)
}

func isRequestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr) || strings.Contains(strings.ToLower(err.Error()), "request body too large")
}

func parseMultipartFormOrRespond(c *gin.Context, cfg *config.Config) bool {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		if isRequestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":      "uploaded file is too large",
				"error_code": "upload_too_large",
				"max_bytes":  cfg.Printing.MaxUploadBytes,
			})
			return false
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid multipart form data",
			"details": err.Error(),
		})
		return false
	}
	return true
}

func cleanupMultipartForm(c *gin.Context) {
	if c.Request != nil && c.Request.MultipartForm != nil {
		_ = c.Request.MultipartForm.RemoveAll()
	}
}

func formFileOrRespond(c *gin.Context, cfg *config.Config) (*multipart.FileHeader, bool) {
	file, err := c.FormFile("file")
	if err != nil {
		if isRequestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":      "uploaded file is too large",
				"error_code": "upload_too_large",
				"max_bytes":  cfg.Printing.MaxUploadBytes,
			})
			return nil, false
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "file is required in multipart form field 'file'",
		})
		return nil, false
	}
	if file.Size > cfg.Printing.MaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":      "uploaded file is too large",
			"error_code": "upload_too_large",
			"max_bytes":  cfg.Printing.MaxUploadBytes,
		})
		return nil, false
	}
	return file, true
}

func validateCopiesOrRespond(c *gin.Context, cfg *config.Config, copies int) bool {
	if copies <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "copies must be a positive integer",
		})
		return false
	}
	if copies > cfg.Printing.MaxCopies {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      fmt.Sprintf("copies must be no more than %d", cfg.Printing.MaxCopies),
			"error_code": "copies_limit_exceeded",
			"max_copies": cfg.Printing.MaxCopies,
		})
		return false
	}
	return true
}

func uploadWorkDir(cfg *config.Config, filename string) string {
	if cfg != nil && (conversion.IsOfficeConvertible(cfg, filename) || conversion.IsImageConvertible(filename)) {
		dir := strings.TrimSpace(cfg.OfficeConversion.OutputDir)
		if dir != "" {
			return dir
		}
	}
	return tempDir()
}

func saveUploadedToDir(c *gin.Context, file *multipart.FileHeader, dir, prefix string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create upload temp dir: %w", err)
	}

	ext := filepath.Ext(file.Filename)
	tmpFile, err := os.CreateTemp(dir, prefix+"-*"+ext)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := c.SaveUploadedFile(file, tempPath); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	return tempPath, nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func officeSourceCachePath(cfg *config.Config, filename, hash string) string {
	ext := filepath.Ext(strings.TrimSpace(filename))
	return filepath.Join(cfg.OfficeConversion.OutputDir, "cache", "source", hash+strings.ToLower(ext))
}

func officePDFCachePath(cfg *config.Config, hash string) string {
	return filepath.Join(cfg.OfficeConversion.OutputDir, "cache", "pdf", hash+".pdf")
}

func prepareUploadedSource(c *gin.Context, cfg *config.Config, file *multipart.FileHeader, prefix string) (string, func(), string, error) {
	tempPath, err := saveUploadedToDir(c, file, uploadWorkDir(cfg, file.Filename), prefix)
	if err != nil {
		return "", nil, "", err
	}

	cleanup := func() {
		_ = os.Remove(tempPath)
	}

	if !(conversion.IsOfficeConvertible(cfg, file.Filename) || conversion.IsImageConvertible(file.Filename)) {
		return tempPath, cleanup, "", nil
	}

	hash, err := fileSHA256(tempPath)
	if err != nil {
		cleanup()
		return "", nil, "", fmt.Errorf("failed to hash uploaded source file: %w", err)
	}

	sourceCachePath := officeSourceCachePath(cfg, file.Filename, hash)
	if err := os.MkdirAll(filepath.Dir(sourceCachePath), 0o755); err != nil {
		cleanup()
		return "", nil, "", fmt.Errorf("failed to create source cache dir: %w", err)
	}

	if _, statErr := os.Stat(sourceCachePath); statErr == nil {
		cleanup()
	} else if os.IsNotExist(statErr) {
		if err := os.Rename(tempPath, sourceCachePath); err != nil {
			cleanup()
			return "", nil, "", fmt.Errorf("failed to persist uploaded source: %w", err)
		}
	} else {
		cleanup()
		return "", nil, "", fmt.Errorf("failed to check source cache: %w", statErr)
	}

	return sourceCachePath, func() {}, hash, nil
}

func ensureCachedConvertedPDF(ctx context.Context, cfg *config.Config, sourcePath, sourceHash string) (string, error) {
	cachedPDFPath := officePDFCachePath(cfg, sourceHash)
	if info, err := os.Stat(cachedPDFPath); err == nil && !info.IsDir() {
		return cachedPDFPath, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to inspect cached pdf: %w", err)
	}

	convertedPath := ""
	var err error

	if conversion.IsOfficeConvertible(cfg, sourcePath) {
		convertedPath, err = conversion.ConvertOfficeToPDF(ctx, cfg, sourcePath)
		if err != nil {
			return "", err
		}
	} else if conversion.IsImageConvertible(sourcePath) {
		convertedPath, err = conversion.ConvertImageToPDF(cfg, sourcePath)
		if err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("source file type is not convertible to pdf")
	}

	if err := os.MkdirAll(filepath.Dir(cachedPDFPath), 0o755); err != nil {
		_ = os.Remove(convertedPath)
		return "", fmt.Errorf("failed to create pdf cache dir: %w", err)
	}
	if err := os.Rename(convertedPath, cachedPDFPath); err != nil {
		_ = os.Remove(convertedPath)
		return "", fmt.Errorf("failed to persist converted pdf: %w", err)
	}

	return cachedPDFPath, nil
}

// ListPrinters 列出所有可用打印机
func ListPrinters(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	configured := cfg.VisiblePrinters()
	printers := make([]gin.H, 0, len(configured))
	for _, printerCfg := range configured {
		cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		printer, err := cupsClient.GetPrinterDetails(printerName)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "Cannot connect to configured printer",
				"printer_id": printerCfg.ID,
				"details":    err.Error(),
			})
			return
		}

		printer.ID = printerCfg.ID
		printers = append(printers, gin.H{
			"id":                  printer.ID,
			"name":                printer.Name,
			"description":         printer.Description,
			"status":              printer.Status,
			"model":               printer.Model,
			"location":            printer.Location,
			"duplex_mode":         printerCfg.NormalizedDuplexMode(),
			"reverse":             printerCfg.Reverse,
			"first_pass":          printerCfg.NormalizedFirstPass(),
			"pad_to_even":         printerCfg.PadToEvenEnabled(),
			"reverse_first_pass":  printerCfg.ReverseFirstPass,
			"reverse_second_pass": printerCfg.ReverseSecondPass,
			"rotate_second_pass":  printerCfg.RotateSecondPass,
			"note":                printerCfg.Note,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"printers": printers,
		"count":    len(printers),
	})
}

// GetPrinterInfo 获取指定打印机的详细信息
func GetPrinterInfo(c *gin.Context) {
	printerID := c.Param("id")
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	printerCfg, err := resolveVisiblePrinter(printerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error(), "printer_id": printerID})
		return
	}

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "printer_id": printerID})
		return
	}

	printer, err := cupsClient.GetPrinterDetails(printerName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":      "Printer not found or CUPS service unavailable",
			"printer_id": printerID,
			"details":    err.Error(),
		})
		return
	}
	printer.ID = printerCfg.ID

	response := gin.H{
		"id":                  printer.ID,
		"name":                printer.Name,
		"description":         printer.Description,
		"status":              printer.Status,
		"model":               printer.Model,
		"location":            printer.Location,
		"duplex_mode":         printerCfg.NormalizedDuplexMode(),
		"reverse":             printerCfg.Reverse,
		"first_pass":          printerCfg.NormalizedFirstPass(),
		"pad_to_even":         printerCfg.PadToEvenEnabled(),
		"reverse_first_pass":  printerCfg.ReverseFirstPass,
		"reverse_second_pass": printerCfg.ReverseSecondPass,
		"rotate_second_pass":  printerCfg.RotateSecondPass,
		"note":                printerCfg.Note,
	}
	if warning := activeJobWarningForPrinter(c.Request.Context(), cfg, printerID); warning != nil {
		response["active_job_warning"] = warning
	}

	c.JSON(http.StatusOK, response)
}

func activeJobWarningForPrinter(ctx context.Context, cfg *config.Config, printerID string) *printerActiveJobWarning {
	if cfg == nil || !cfg.JobStore.Enabled {
		return nil
	}
	store, err := newBitableJobStore(cfg)
	if err != nil {
		log.Printf("[printer] skip active job warning printer=%s err=%v", printerID, err)
		return nil
	}
	warning, err := store.ActiveWarningForPrinter(ctx, printerID, time.Now())
	if err != nil {
		log.Printf("[printer] active job warning query failed printer=%s err=%v", printerID, err)
		return nil
	}
	return warning
}

// GetSupportedFileTypes 返回当前支持上传/转换的文件扩展名（不带点），始终包含 pdf。
func GetSupportedFileTypes(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	fileTypes := conversion.SupportedUploadExtensions(cfg)

	c.JSON(http.StatusOK, gin.H{
		"supported_file_types":      fileTypes,
		"count":                     len(fileTypes),
		"office_conversion_enabled": cfg.OfficeConversion.Enabled,
	})
}

// SubmitPrintJob 提交打印任务
func SubmitPrintJob(c *gin.Context) {
	cfg, cfgErr := requireConfig()
	if cfgErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": cfgErr.Error()})
		return
	}
	limitRequestBody(c, cfg)
	started := time.Now()
	log.Printf("[print] request start remote=%s content_length=%d", c.ClientIP(), c.Request.ContentLength)
	defer func() {
		log.Printf("[print] request finished status=%d duration=%s", c.Writer.Status(), time.Since(started).Round(time.Millisecond))
	}()
	if !parseMultipartFormOrRespond(c, cfg) {
		return
	}
	defer cleanupMultipartForm(c)

	printerID := c.PostForm("printer_id")
	if printerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "printer_id is required",
		})
		return
	}

	copies := 1
	if rawCopies := c.Query("copies"); rawCopies != "" {
		parsedCopies, err := strconv.Atoi(rawCopies)
		if err != nil || parsedCopies <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "copies must be a positive integer",
			})
			return
		}
		copies = parsedCopies
	}
	if !validateCopiesOrRespond(c, cfg, copies) {
		return
	}

	duplexRequested := false
	duplex := "long-edge" // 默认长边翻转
	if rawDuplex := strings.TrimSpace(strings.ToLower(c.Query("duplex"))); rawDuplex != "" {
		switch rawDuplex {
		case "1", "true", "yes", "on":
			duplexRequested = true
			duplex = "long-edge"
		case "0", "false", "no", "off":
			duplexRequested = false
		case "long-edge", "short-edge":
			duplexRequested = true
			duplex = rawDuplex
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "duplex must be one of: true/false/1/0/yes/no/on/off/long-edge/short-edge",
			})
			return
		}
	}

	collate := true
	if rawCollate := strings.TrimSpace(strings.ToLower(c.Query("collate"))); rawCollate != "" {
		switch rawCollate {
		case "1", "true", "yes", "on":
			collate = true
		case "0", "false", "no", "off":
			collate = false
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "collate must be one of: true/false/1/0/yes/no/on/off",
			})
			return
		}
	}

	nup := 1
	if rawNup := strings.TrimSpace(c.Query("nup")); rawNup != "" {
		parsedNup, err := strconv.Atoi(rawNup)
		if err != nil || (parsedNup != 1 && parsedNup != 2 && parsedNup != 4 && parsedNup != 6) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "nup must be 1, 2, 4, or 6",
			})
			return
		}
		nup = parsedNup
	}

	rawScale := c.Query("scale")
	if strings.TrimSpace(rawScale) == "" {
		rawScale = c.PostForm("scale")
	}
	scalePercent, err := parseScalePercent(rawScale)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	pages := strings.TrimSpace(c.Query("pages"))

	file, ok := formFileOrRespond(c, cfg)
	if !ok {
		return
	}

	if !conversion.IsSupportedUploadFile(cfg, file.Filename) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "unsupported file type, accepted formats are office_conversion.accepted_formats plus pdf/jpg/jpeg/png",
			"error_code": "unsupported_file_type",
		})
		return
	}

	queueStarted := time.Now()
	log.Printf("[print] waiting submit queue printer=%s filename=%q size=%d", printerID, file.Filename, file.Size)
	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		log.Printf("[print] submit queue unavailable printer=%s filename=%q err=%v", printerID, file.Filename, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "print queue is busy or request cancelled",
			"error_code": "print_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	log.Printf("[print] submit queue acquired printer=%s filename=%q wait=%s", printerID, file.Filename, time.Since(queueStarted).Round(time.Millisecond))
	defer releasePrintSubmitQueue()

	tempPath, cleanupUploaded, sourceHash, err := prepareUploadedSource(c, cfg, file, "goprint")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to save uploaded file",
			"details": err.Error(),
		})
		return
	}
	defer cleanupUploaded()

	printSourcePath := tempPath
	convertedPath := ""
	if conversion.IsOfficeConvertible(cfg, file.Filename) || conversion.IsImageConvertible(file.Filename) {
		convertedPath, err = ensureCachedConvertedPDF(c.Request.Context(), cfg, tempPath, sourceHash)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "failed to convert source file to pdf",
				"error_code": "file_conversion_failed",
				"details":    err.Error(),
			})
			return
		}
		printSourcePath = convertedPath
	}

	if _, err := enforcePDFPageLimit(cfg, printSourcePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}

	pageSelectionCleanup := func() {}
	if pages != "" {
		selectedPath, cleanupPages, err := extractPDFPages(printSourcePath, pages)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to extract specified pages",
				"details": err.Error(),
			})
			return
		}
		printSourcePath = selectedPath
		pageSelectionCleanup = cleanupPages
	}
	defer pageSelectionCleanup()

	nupCleanup := func() {}
	if nup > 1 {
		nupPath, cleanupNup, err := applyNupLayout(printSourcePath, nup, "horizontal")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to apply nup layout",
				"details": err.Error(),
			})
			return
		}
		if nupPath != printSourcePath {
			pageSelectionCleanup()
			printSourcePath = nupPath
			nupCleanup = cleanupNup
		}
	}
	defer nupCleanup()

	scaleCleanup := func() {}
	if scalePercent != 100 {
		scaledPath, cleanupScale, err := applyScalePercent(printSourcePath, scalePercent)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to apply page scale",
				"details": err.Error(),
			})
			return
		}
		if scaledPath != printSourcePath {
			printSourcePath = scaledPath
			scaleCleanup = cleanupScale
		}
	}
	defer scaleCleanup()

	printerCfg, err := resolveVisiblePrinter(printerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error(), "printer_id": printerID})
		return
	}

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "printer_id": printerID})
		return
	}

	duplexMode := printerCfg.NormalizedDuplexMode()
	if !duplexRequested {
		duplexMode = "off"
	}
	printPageCount := 0
	if pageCount, countErr := countPDFPages(printSourcePath); countErr == nil {
		printPageCount = pageCount
		if pageCount == 1 {
			duplexMode = "off"
		}
	} else {
		log.Printf("[print] failed to count pages path=%s err=%v", printSourcePath, countErr)
	}

	if duplexRequested && printerCfg.NormalizedDuplexMode() == "off" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "duplex requested but printer duplex_mode is off",
			"printer_id": printerID,
			"printer_config": gin.H{
				"duplex_mode":         printerCfg.NormalizedDuplexMode(),
				"reverse":             printerCfg.Reverse,
				"first_pass":          printerCfg.NormalizedFirstPass(),
				"pad_to_even":         printerCfg.PadToEvenEnabled(),
				"reverse_first_pass":  printerCfg.ReverseFirstPass,
				"reverse_second_pass": printerCfg.ReverseSecondPass,
				"rotate_second_pass":  printerCfg.RotateSecondPass,
				"note":                printerCfg.Note,
			},
			"hint": "set duplex_mode to auto or manual in config.yaml if this printer supports duplex",
		})
		return
	}

	if duplexMode == "manual" {
		duplexEdge := extractDuplexEdge(duplex)
		firstPassPath, secondPassPath, cleanup, err := prepareManualDuplexFiles(printSourcePath, printerCfg, duplexEdge)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to prepare manual duplex files",
				"details": err.Error(),
			})
			return
		}
		defer cleanup()

		firstPassToSubmit, err := applyCopiesMode(firstPassPath, copies, collate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to build first pass copies",
				"details": err.Error(),
			})
			return
		}
		if firstPassToSubmit != firstPassPath {
			defer os.Remove(firstPassToSubmit)
		}

		secondPassToStore, err := applyCopiesMode(secondPassPath, copies, collate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to build second pass copies",
				"details": err.Error(),
			})
			return
		}

		initialJobID, err := cupsClient.SubmitJob(printerName, firstPassToSubmit, cups.PrintOptions{Copies: 1})
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "failed to submit first pass for manual duplex",
				"printer_id": printerID,
				"details":    err.Error(),
			})
			return
		}

		token, expiresAt, err := saveManualDuplexPending(initialJobID, printerID, secondPassToStore, 1, "", printPageCount*copies)
		if err != nil {
			_ = os.Remove(secondPassToStore)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "failed to create manual duplex hook",
				"details": err.Error(),
			})
			return
		}

		hookURL := fmt.Sprintf("/manual-duplex-hooks/%s/continue", token)

		c.JSON(http.StatusCreated, gin.H{
			"job_id":                     initialJobID,
			"printer":                    printerID,
			"copies":                     copies,
			"collate":                    collate,
			"status":                     "pending",
			"duplex":                     true,
			"note":                       printerCfg.Note,
			"message":                    "First pass submitted. Use hook_url to print remaining pages.",
			"hook_url":                   hookURL,
			"hook_expires_at":            expiresAt.In(time.Local).Format("2006-01-02 15:04"),
			"hook_extend_window_seconds": manualDuplexExtendWindowSeconds(),
		})

		if user, ok := currentAuthUser(c); ok {
			cfg, cfgErr := requireConfig()
			if cfgErr == nil {
				persistPrintJobToBitable(c, cfg, printJobRecord{
					JobID:          initialJobID,
					PrinterID:      printerID,
					FileName:       file.Filename,
					Status:         "pending_manual_continue",
					Copies:         copies,
					PageCount:      printPageCount,
					Duplex:         true,
					DuplexHook:     hookURL,
					DuplexExpireAt: expiresAt,
					User:           user,
				})

				// 将任务注册到后台轮询器
				tracker := initJobStatusPoller(cfg)
				if tracker != nil {
					tracker.AddPendingJobWithStatus(initialJobID, printerID, "pending_manual_continue")
				}
			}
		}
		return
	}

	printPath := printSourcePath
	if duplexMode == "off" && printerCfg.Reverse {
		reversedPath, reverseErr := prepareReversedPDF(printSourcePath)
		if reverseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to reverse document for single-side printing",
				"details": reverseErr.Error(),
			})
			return
		}
		if reversedPath != printSourcePath {
			defer os.Remove(reversedPath)
			printPath = reversedPath
		}
	}

	finalPrintPath, err := applyCopiesMode(printPath, copies, collate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "failed to build copies",
			"details": err.Error(),
		})
		return
	}
	if finalPrintPath != printPath {
		defer os.Remove(finalPrintPath)
	}

	printOpts := cups.PrintOptions{Copies: 1, Collate: collate}
	if duplexMode == "auto" {
		autoSides, sideErr := chooseAutoDuplexSides(finalPrintPath)
		if sideErr != nil {
			log.Printf("[print] failed to detect document orientation, fallback to long-edge: %v", sideErr)
			printOpts.Sides = "two-sided-long-edge"
		} else {
			printOpts.Sides = autoSides
		}
	}

	jobID, err := cupsClient.SubmitJob(printerName, finalPrintPath, printOpts)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "failed to submit print job",
			"printer_id": printerID,
			"details":    err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"job_id":  jobID,
		"printer": printerID,
		"copies":  copies,
		"collate": collate,
		"status":  "pending",
		"duplex":  duplexMode != "off",
		"note":    printerCfg.Note,
		"message": "Print job submitted successfully",
	})

	if user, ok := currentAuthUser(c); ok {
		cfg, cfgErr := requireConfig()
		if cfgErr == nil {
			persistPrintJobToBitable(c, cfg, printJobRecord{
				JobID:     jobID,
				PrinterID: printerID,
				FileName:  file.Filename,
				Status:    "pending",
				Copies:    copies,
				PageCount: printPageCount,
				Duplex:    duplexMode != "off",
				User:      user,
			})

			// 将任务注册到后台轮询器
			tracker := initJobStatusPoller(cfg)
			if tracker != nil {
				tracker.AddPendingJob(jobID, printerID)
			}
		}
	}
}

// PreviewConvertedDocument 仅转换文档并返回 PDF 用于前端预览，不提交打印任务。
func PreviewConvertedDocument(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	limitRequestBody(c, cfg)
	started := time.Now()
	log.Printf("[preview] request start remote=%s content_length=%d", c.ClientIP(), c.Request.ContentLength)
	defer func() {
		log.Printf("[preview] request finished status=%d duration=%s", c.Writer.Status(), time.Since(started).Round(time.Millisecond))
	}()
	if !parseMultipartFormOrRespond(c, cfg) {
		return
	}
	defer cleanupMultipartForm(c)

	file, ok := formFileOrRespond(c, cfg)
	if !ok {
		return
	}
	log.Printf("[preview] uploaded filename=%q size=%d", file.Filename, file.Size)

	if !conversion.IsSupportedUploadFile(cfg, file.Filename) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "unsupported file type, accepted formats are office_conversion.accepted_formats plus pdf/jpg/jpeg/png",
			"error_code": "unsupported_file_type",
		})
		return
	}

	queueStarted := time.Now()
	log.Printf("[preview] waiting submit queue filename=%q size=%d", file.Filename, file.Size)
	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		log.Printf("[preview] submit queue unavailable filename=%q err=%v", file.Filename, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "preview queue is busy or request cancelled",
			"error_code": "preview_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	log.Printf("[preview] submit queue acquired filename=%q wait=%s", file.Filename, time.Since(queueStarted).Round(time.Millisecond))
	defer releasePrintSubmitQueue()

	tempPath, cleanupUploaded, sourceHash, err := prepareUploadedSource(c, cfg, file, "goprint-preview")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to save uploaded file",
			"details": err.Error(),
		})
		return
	}
	defer cleanupUploaded()

	previewPath := tempPath
	if conversion.IsOfficeConvertible(cfg, file.Filename) || conversion.IsImageConvertible(file.Filename) {
		convertedPath, convErr := ensureCachedConvertedPDF(c.Request.Context(), cfg, tempPath, sourceHash)
		if convErr != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "failed to convert source file to pdf",
				"error_code": "file_conversion_failed",
				"details":    convErr.Error(),
			})
			return
		}
		previewPath = convertedPath
	}

	if _, err := enforcePDFPageLimit(cfg, previewPath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}

	previewName := strings.TrimSuffix(filepath.Base(file.Filename), filepath.Ext(file.Filename)) + ".pdf"
	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", previewName))
	c.File(previewPath)
}

func ContinueManualDuplexPrint(c *gin.Context) {
	token := c.Param("token")
	pending, ok := claimManualDuplexPending(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "manual duplex hook not found, already used, or already processing",
		})
		return
	}

	printerCfg, err := resolvePrinter(pending.PrinterID)
	if err != nil {
		releaseManualDuplexPending(token)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error(), "printer_id": pending.PrinterID})
		return
	}

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		releaseManualDuplexPending(token)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "printer_id": pending.PrinterID})
		return
	}

	jobID, err := cupsClient.SubmitJob(printerName, pending.RemainingFilePath, cups.PrintOptions{Copies: pending.Copies})
	if err != nil {
		releaseManualDuplexPending(token)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "failed to submit remaining pages",
			"details": err.Error(),
		})
		return
	}

	_ = os.Remove(pending.RemainingFilePath)
	deleteManualDuplexPending(token)

	c.JSON(http.StatusCreated, gin.H{
		"job_id":  jobID,
		"printer": pending.PrinterID,
		"status":  "pending",
		"duplex":  true,
		"message": "Remaining pages submitted successfully",
	})

	cfg, cfgErr := requireConfig()
	if cfgErr == nil {
		go func() {
			if store, storeErr := newBitableJobStore(cfg); storeErr == nil {
				if err := store.UpdateManualDuplexContinued(context.Background(), pending.JobID, jobID); err != nil {
					log.Printf("[manual-duplex] failed to update continued job in bitable initial_job_id=%s continued_job_id=%s err=%v", pending.JobID, jobID, err)
				}
			} else {
				log.Printf("[manual-duplex] bitable store init failed while continuing initial_job_id=%s continued_job_id=%s err=%v", pending.JobID, jobID, storeErr)
			}

			// 将第二遍任务也注册到后台轮询器
			tracker := initJobStatusPoller(cfg)
			if tracker != nil {
				tracker.AddPendingJob(jobID, pending.PrinterID)
			}
		}()
	}
}

func ExtendManualDuplexPrint(c *gin.Context) {
	token := c.Param("token")
	pending, ok, err := extendManualDuplexPending(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "manual duplex hook not found, expired, already used, or already processing",
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":                      "manual duplex hook cannot be extended yet",
			"details":                    err.Error(),
			"printer":                    pending.PrinterID,
			"duplex":                     true,
			"status":                     "pending_manual_continue",
			"hook_expires_at":            pending.ExpiresAt.In(time.Local).Format("2006-01-02 15:04"),
			"hook_extend_window_seconds": manualDuplexExtendWindowSeconds(),
		})
		return
	}

	syncManualDuplexExpireAtToBitable(pending.JobID, pending.ExpiresAt)

	c.JSON(http.StatusOK, gin.H{
		"printer":                    pending.PrinterID,
		"duplex":                     true,
		"status":                     "pending_manual_continue",
		"message":                    "Manual duplex hook extended",
		"hook_expires_at":            pending.ExpiresAt.In(time.Local).Format("2006-01-02 15:04"),
		"hook_extend_window_seconds": manualDuplexExtendWindowSeconds(),
	})
}

func CancelManualDuplexPrint(c *gin.Context) {
	token := c.Param("token")
	pending, ok := claimManualDuplexPending(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "manual duplex hook not found, already used, or already processing",
		})
		return
	}

	cfg, err := requireConfig()
	if err != nil {
		releaseManualDuplexPending(token)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if tracker := initJobStatusPoller(cfg); tracker != nil && pending.JobID != "" {
		tracker.RemovePendingJob(pending.JobID)
	}

	_ = os.Remove(pending.RemainingFilePath)
	deleteManualDuplexPending(token)

	c.JSON(http.StatusOK, gin.H{
		"printer":   pending.PrinterID,
		"duplex":    false,
		"status":    "cancelled",
		"message":   "Manual duplex flow cancelled; remaining pages were not submitted",
		"cancelled": true,
	})

	go func() {
		store, err := newBitableJobStore(cfg)
		if err != nil {
			log.Printf("[manual-duplex] skip bitable cancel update job_id=%s err=%v", pending.JobID, err)
			return
		}
		if pending.JobID != "" {
			if err := store.UpdateJobStatus(context.Background(), pending.JobID, "cancelled"); err != nil {
				log.Printf("[manual-duplex] failed to mark job cancelled in bitable job_id=%s err=%v", pending.JobID, err)
			}
		}
	}()
}

// ListPrintJobs 列出所有打印任务
func ListPrintJobs(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authenticated user context"})
		return
	}

	store, err := newBitableJobStore(cfg)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "job store is not available",
			"details": err.Error(),
		})
		return
	}

	printerID := c.Query("printer_id")
	allJobs, err := store.ListJobsByUser(context.Background(), user, 500)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query jobs from bitable", "details": err.Error()})
		return
	}

	jobs := make([]map[string]interface{}, 0, len(allJobs))
	for _, job := range allJobs {
		if printerID != "" {
			if fmt.Sprint(job["printer"]) != printerID {
				continue
			}
		}
		jobs = append(jobs, job)
	}

	c.JSON(http.StatusOK, gin.H{
		"jobs":  jobs,
		"count": len(jobs),
	})
}

// GetJobStatus 获取打印任务状态
func GetJobStatus(c *gin.Context) {
	jobIDStr := c.Param("id")
	jobID, err := strconv.Atoi(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "Invalid job ID format",
			"job_id": jobIDStr,
		})
		return
	}

	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var job *cups.PrintJob
	for _, printerCfg := range cfg.Printers {
		cupsClient, _, clientErr := newCupsClientForPrinter(printerCfg)
		if clientErr != nil {
			continue
		}

		candidate, queryErr := cupsClient.GetPrintJobDetails(jobID)
		if queryErr == nil {
			candidate.PrinterID = printerCfg.ID
			job = candidate
			break
		}
	}

	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "Job not found or CUPS service unavailable",
			"job_id":  jobIDStr,
			"details": "unable to resolve job from configured printers",
		})
		return
	}

	c.JSON(http.StatusOK, job)
}

// CancelPrintJob 取消打印任务
func CancelPrintJob(c *gin.Context) {
	jobIDStr := c.Param("id")
	if strings.TrimSpace(jobIDStr) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "Invalid job ID format",
			"job_id": jobIDStr,
		})
		return
	}

	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authenticated user context"})
		return
	}

	store, err := newBitableJobStore(cfg)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "job store is not available",
			"details": err.Error(),
		})
		return
	}

	deleted, err := store.DeleteJobByUserAndJobID(context.Background(), user, jobIDStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to delete print job from bitable",
			"job_id":  jobIDStr,
			"details": err.Error(),
		})
		return
	}

	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{
			"error":  "job not found",
			"job_id": jobIDStr,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":  jobIDStr,
		"status":  "deleted",
		"message": "Task removed from bitable only. Please cancel the physical print job on printer panel if needed.",
	})
}
