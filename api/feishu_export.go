package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"goprint/api/pdfutil"
	"goprint/config"
	"goprint/cups"

	"github.com/gin-gonic/gin"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	drivev1 "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	wikiv2 "github.com/larksuite/oapi-sdk-go/v3/service/wiki/v2"
)

const (
	feishuExportPollInterval = 2 * time.Second
	feishuExportMaxPolls     = 60 // max ~2 minutes
)

type feishuDocInfo struct {
	Token    string
	Type     string
	Filename string
}

type feishuExportRequest struct {
	URL string `json:"url"`
}

var feishuErrCodeRE = regexp.MustCompile(`code=(\d+)\b`)

// feishuExportHTTPStatus extracts a Feishu API error code from err and maps it
// to an HTTP status code. When the upstream returns a 4xx-class error, we
// forward a corresponding 4xx status to the caller.
func feishuExportHTTPStatus(err error) int {
	if err == nil {
		return http.StatusServiceUnavailable
	}

	m := feishuErrCodeRE.FindStringSubmatch(err.Error())
	if m == nil {
		return http.StatusServiceUnavailable
	}

	switch m[1] {
	case "131005": // wiki node not found
		return http.StatusNotFound
	case "1069906": // document deleted
		return http.StatusGone
	case "1069902", "131006": // permission denied (drive / wiki)
		return http.StatusForbidden
	case "1069904", "1069905": // invalid parameter / invalid token
		return http.StatusBadRequest
	case "1069901", "1069903": // internal error / export task failed
		return http.StatusInternalServerError
	default:
		return http.StatusServiceUnavailable
	}
}

func newFeishuClient(cfg *config.Config) (*lark.Client, error) {
	appID := strings.TrimSpace(cfg.Auth.Feishu.AppID)
	appSecret := strings.TrimSpace(cfg.Auth.Feishu.AppSecret)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("feishu app_id/app_secret not configured")
	}
	return lark.NewClient(appID, appSecret), nil
}

func extractUserAccessToken(c *gin.Context) string {
	raw, ok := c.Get("auth_token")
	if !ok {
		return ""
	}
	token, ok := raw.(string)
	if !ok {
		return ""
	}
	return token
}

var feishuDocURLRE = regexp.MustCompile(
	`https?://[^/]+\.feishu\.cn/(docx|doc|sheets|bitable|mindnotes)/([A-Za-z0-9_-]+)`)

var feishuWikiURLRE = regexp.MustCompile(
	`https?://[^/]+\.feishu\.cn/wiki/([A-Za-z0-9_-]+)`)

func parseFeishuURL(rawURL string) (docType string, token string, err error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", fmt.Errorf("url is empty")
	}

	if m := feishuDocURLRE.FindStringSubmatch(rawURL); m != nil {
		return m[1], m[2], nil
	}
	if m := feishuWikiURLRE.FindStringSubmatch(rawURL); m != nil {
		return "wiki", m[1], nil
	}

	return "", "", fmt.Errorf("unsupported feishu url: %s", rawURL)
}

func resolveWikiNode(ctx context.Context, client *lark.Client, userAccessToken string, nodeToken string) (*feishuDocInfo, error) {
	req := wikiv2.NewGetNodeSpaceReqBuilder().
		Token(nodeToken).
		Build()

	resp, err := client.Wiki.V2.Space.GetNode(ctx, req, larkcore.WithUserAccessToken(userAccessToken))
	if err != nil {
		return nil, fmt.Errorf("wiki get_node call failed: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("wiki get_node error: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Node == nil {
		return nil, fmt.Errorf("wiki get_node returned empty data")
	}

	node := resp.Data.Node
	objToken := ""
	objType := ""
	title := ""

	if node.ObjToken != nil {
		objToken = *node.ObjToken
	}
	if node.ObjType != nil {
		objType = *node.ObjType
	}
	if node.Title != nil {
		title = *node.Title
	}

	if objToken == "" {
		return nil, fmt.Errorf("wiki node has no obj_token (node may be a folder, shortcut, or empty)")
	}

	if node.NodeType != nil && *node.NodeType == "shortcut" {
		if node.OriginNodeToken != nil {
			log.Printf("[feishu-export] following wiki shortcut %s -> %s", nodeToken, *node.OriginNodeToken)
			return resolveWikiNode(ctx, client, userAccessToken, *node.OriginNodeToken)
		}
	}

	log.Printf("[feishu-export] resolved wiki node %s -> obj_token=%s obj_type=%s title=%s",
		nodeToken, maskSensitive(objToken), objType, title)

	return &feishuDocInfo{
		Token:    objToken,
		Type:     objType,
		Filename: title,
	}, nil
}

func createExportTask(ctx context.Context, client *lark.Client, userAccessToken string, doc *feishuDocInfo) (string, error) {
	task := drivev1.NewExportTaskBuilder().
		FileExtension("pdf").
		Token(doc.Token).
		Type(doc.Type).
		Build()

	req := drivev1.NewCreateExportTaskReqBuilder().
		ExportTask(task).
		Build()

	var opts []larkcore.RequestOptionFunc
	if userAccessToken != "" {
		opts = append(opts, larkcore.WithUserAccessToken(userAccessToken))
	}
	resp, err := client.Drive.V1.ExportTask.Create(ctx, req, opts...)
	if err != nil {
		return "", fmt.Errorf("export task create call failed: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("export task create error: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Ticket == nil {
		return "", fmt.Errorf("export task create returned empty ticket")
	}

	ticket := *resp.Data.Ticket
	log.Printf("[feishu-export] created export task ticket=%s token=%s type=%s", ticket, maskSensitive(doc.Token), doc.Type)
	return ticket, nil
}

func pollExportTask(ctx context.Context, client *lark.Client, userAccessToken string, ticket string, token string) (string, error) {
	for i := 0; i < feishuExportMaxPolls; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(feishuExportPollInterval):
		}

		req := drivev1.NewGetExportTaskReqBuilder().
			Ticket(ticket).
			Token(token).
			Build()

		var opts []larkcore.RequestOptionFunc
		if userAccessToken != "" {
			opts = append(opts, larkcore.WithUserAccessToken(userAccessToken))
		}
		resp, err := client.Drive.V1.ExportTask.Get(ctx, req, opts...)
		if err != nil {
			log.Printf("[feishu-export] poll %d/%d ticket=%s call failed: %v", i+1, feishuExportMaxPolls, ticket, err)
			continue
		}

		if !resp.Success() {
			log.Printf("[feishu-export] poll %d/%d ticket=%s api error code=%d msg=%s", i+1, feishuExportMaxPolls, ticket, resp.Code, resp.Msg)
			continue
		}

		if resp.Data == nil || resp.Data.Result == nil {
			log.Printf("[feishu-export] poll %d/%d ticket=%s empty data (Data=%v)", i+1, feishuExportMaxPolls, ticket, resp.Data != nil)
			continue
		}

		result := resp.Data.Result
		jobStatus := 1
		if result.JobStatus != nil {
			jobStatus = *result.JobStatus
		}

		log.Printf("[feishu-export] poll %d/%d ticket=%s job_status=%d job_error_msg=%q file_token=%s file_size=%s",
			i+1, feishuExportMaxPolls, ticket, jobStatus,
			stringPtr(result.JobErrorMsg),
			stringPtr(result.FileToken),
			intPtr(result.FileSize),
		)

		switch jobStatus {
		case 0: // success
			if result.FileToken == nil || *result.FileToken == "" {
				return "", fmt.Errorf("export completed but no file_token returned")
			}
			fileToken := *result.FileToken
			log.Printf("[feishu-export] export completed ticket=%s file_token=%s", ticket, fileToken)
			return fileToken, nil

		case 1: // initializing
			continue

		case 2: // processing
			continue

		case 3: // internal error
			errMsg := "internal error"
			if result.JobErrorMsg != nil && *result.JobErrorMsg != "" {
				errMsg = *result.JobErrorMsg
			}
			return "", fmt.Errorf("export failed: %s", errMsg)

		case 107: // document too large
			return "", fmt.Errorf("export failed: document too large to export")

		default:
			return "", fmt.Errorf("export unknown job_status=%d", jobStatus)
		}
	}

	return "", fmt.Errorf("export timed out after %d polls", feishuExportMaxPolls)
}

func stringPtr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func intPtr(n *int) string {
	if n == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *n)
}

func downloadExportedFile(ctx context.Context, cfg *config.Config, client *lark.Client, userAccessToken string, fileToken string, filename string) (string, error) {
	req := drivev1.NewDownloadExportTaskReqBuilder().
		FileToken(fileToken).
		Build()

	var opts []larkcore.RequestOptionFunc
	if userAccessToken != "" {
		opts = append(opts, larkcore.WithUserAccessToken(userAccessToken))
	}
	resp, err := client.Drive.V1.ExportTask.Download(ctx, req, opts...)
	if err != nil {
		return "", fmt.Errorf("download call failed: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("download error: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.File == nil {
		return "", fmt.Errorf("download returned empty file body")
	}

	f, err := os.CreateTemp(tempDir(), "feishu-export-*.pdf")
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	outPath := f.Name()
	defer f.Close()

	if closer, ok := interface{}(resp.File).(io.Closer); ok {
		defer closer.Close()
	}

	maxBytes := cfg.Printing.MaxUploadBytes
	written, err := io.Copy(f, io.LimitReader(resp.File, maxBytes+1))
	if err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("failed to write downloaded file: %w", err)
	}
	if written > maxBytes {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("exported pdf exceeds configured size limit of %d bytes", maxBytes)
	}

	log.Printf("[feishu-export] downloaded file_token=%s to %s bytes=%d", fileToken, outPath, written)
	return outPath, nil
}

func exportFeishuDocToPDF(ctx context.Context, cfg *config.Config, client *lark.Client, userAccessToken string, rawURL string) (pdfPath string, filename string, err error) {
	docType, token, err := parseFeishuURL(rawURL)
	if err != nil {
		return "", "", err
	}

	doc := &feishuDocInfo{Token: token, Type: docType}

	if docType == "wiki" {
		resolved, resolveErr := resolveWikiNode(ctx, client, userAccessToken, token)
		if resolveErr != nil {
			return "", "", fmt.Errorf("failed to resolve wiki node: %w", resolveErr)
		}
		doc = resolved
	}

	ticket, err := createExportTask(ctx, client, userAccessToken, doc)
	if err != nil {
		return "", "", err
	}

	fileToken, err := pollExportTask(ctx, client, userAccessToken, ticket, doc.Token)
	if err != nil {
		return "", "", err
	}

	pdfPath, err = downloadExportedFile(ctx, cfg, client, userAccessToken, fileToken, doc.Filename)
	if err != nil {
		return "", "", err
	}

	return pdfPath, doc.Filename, nil
}

// PreviewFeishuDocument handles POST /api/jobs/preview/feishu.
func PreviewFeishuDocument(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	started := time.Now()
	log.Printf("[feishu-preview] request start remote=%s content_length=%d", c.ClientIP(), c.Request.ContentLength)
	defer func() {
		log.Printf("[feishu-preview] request finished status=%d duration=%s", c.Writer.Status(), time.Since(started).Round(time.Millisecond))
	}()

	userToken := extractUserAccessToken(c)
	if userToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing user access token"})
		return
	}

	var req feishuExportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid request body, expected {\"url\": \"...\"}",
		})
		return
	}

	if strings.TrimSpace(req.URL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	queueStarted := time.Now()
	log.Printf("[feishu-preview] waiting submit queue url=%s", req.URL)
	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		log.Printf("[feishu-preview] submit queue unavailable url=%s err=%v", req.URL, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "preview queue is busy or request cancelled",
			"error_code": "preview_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	log.Printf("[feishu-preview] submit queue acquired url=%s wait=%s", req.URL, time.Since(queueStarted).Round(time.Millisecond))
	defer releasePrintSubmitQueue()

	client, err := newFeishuClient(cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	pdfPath, filename, err := exportFeishuDocToPDF(c.Request.Context(), cfg, client, userToken, req.URL)
	if err != nil {
		log.Printf("[feishu-export] preview export failed url=%s err=%v", req.URL, err)
		c.JSON(feishuExportHTTPStatus(err), gin.H{
			"error":      "failed to export feishu document",
			"error_code": "feishu_export_failed",
			"details":    err.Error(),
		})
		return
	}
	defer os.Remove(pdfPath)

	if _, err := enforcePDFPageLimit(cfg, pdfPath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}

	previewName := "document.pdf"
	if filename != "" {
		previewName = sanitizeFilename(filename) + ".pdf"
	}

	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", previewName))
	c.File(pdfPath)
}

// SubmitFeishuPrintJob handles POST /api/jobs/feishu.
func SubmitFeishuPrintJob(c *gin.Context) {
	userToken := extractUserAccessToken(c)
	if userToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing user access token"})
		return
	}

	var reqBody struct {
		URL       string `json:"url"`
		PrinterID string `json:"printer_id"`
		Copies    int    `json:"copies"`
		Duplex    bool   `json:"duplex"`
		Collate   *bool  `json:"collate"`
		Nup       int    `json:"nup"`
		Scale     int    `json:"scale"`
		Pages     string `json:"pages"`
	}

	if err := c.ShouldBindJSON(&reqBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid request body",
		})
		return
	}

	if strings.TrimSpace(reqBody.URL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	printerID := strings.TrimSpace(reqBody.PrinterID)
	if printerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "printer_id is required"})
		return
	}

	cfg, cfgErr := requireConfig()
	if cfgErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": cfgErr.Error()})
		return
	}
	started := time.Now()
	log.Printf("[feishu-print] request start remote=%s printer=%s content_length=%d", c.ClientIP(), printerID, c.Request.ContentLength)
	defer func() {
		log.Printf("[feishu-print] request finished status=%d duration=%s", c.Writer.Status(), time.Since(started).Round(time.Millisecond))
	}()

	copies := reqBody.Copies
	if copies <= 0 {
		copies = 1
	}
	if !validateCopiesOrRespond(c, cfg, copies) {
		return
	}

	collate := true
	if reqBody.Collate != nil {
		collate = *reqBody.Collate
	}

	nup := reqBody.Nup
	if nup <= 0 {
		nup = 1
	}
	if nup != 1 && !pdfutil.ValidNup(nup) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nup must be 1, 2, 4, or 6"})
		return
	}

	scalePercent := reqBody.Scale
	if scalePercent <= 0 {
		scalePercent = 100
	}
	if scalePercent < 10 || scalePercent > 400 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scale must be an integer between 10 and 400"})
		return
	}

	queueStarted := time.Now()
	log.Printf("[feishu-print] waiting submit queue printer=%s url=%s", printerID, reqBody.URL)
	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		log.Printf("[feishu-print] submit queue unavailable printer=%s url=%s err=%v", printerID, reqBody.URL, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "print queue is busy or request cancelled",
			"error_code": "print_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	log.Printf("[feishu-print] submit queue acquired printer=%s wait=%s", printerID, time.Since(queueStarted).Round(time.Millisecond))
	defer releasePrintSubmitQueue()

	client, err := newFeishuClient(cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	pdfPath, docFilename, err := exportFeishuDocToPDF(c.Request.Context(), cfg, client, userToken, reqBody.URL)
	if err != nil {
		log.Printf("[feishu-export] print export failed url=%s err=%v", reqBody.URL, err)
		c.JSON(feishuExportHTTPStatus(err), gin.H{
			"error":      "failed to export feishu document",
			"error_code": "feishu_export_failed",
			"details":    err.Error(),
		})
		return
	}
	defer os.Remove(pdfPath)

	printSourcePath := pdfPath
	if _, err := enforcePDFPageLimit(cfg, printSourcePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}

	pageSelectionCleanup := func() {}
	if strings.TrimSpace(reqBody.Pages) != "" {
		selectedPath, cleanupPages, err := extractPDFPages(printSourcePath, reqBody.Pages)
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
	if !reqBody.Duplex {
		duplexMode = "off"
	}
	printPageCount := 0
	if pageCount, countErr := countPDFPages(printSourcePath); countErr == nil {
		printPageCount = pageCount
		if pageCount == 1 {
			duplexMode = "off"
		}
	} else {
		log.Printf("[feishu-print] failed to count pages path=%s err=%v", printSourcePath, countErr)
	}

	if reqBody.Duplex && printerCfg.NormalizedDuplexMode() == "off" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "duplex requested but printer duplex_mode is off",
			"printer_id": printerID,
			"hint":       "set duplex_mode to auto or manual in config.yaml if this printer supports duplex",
		})
		return
	}

	if duplexMode == "manual" {
		// 检测文档方向以确定使用长边还是短边
		duplexEdge := "long-edge"
		if detectedSides, err := chooseAutoDuplexSides(printSourcePath); err == nil {
			duplexEdge = extractDuplexEdge(detectedSides)
		}
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

		filename := docFilename
		if filename == "" {
			filename = "feishu_document.pdf"
		}

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
			persistPrintJobToBitable(c, cfg, printJobRecord{
				JobID:          initialJobID,
				PrinterID:      printerID,
				FileName:       filename,
				Status:         "pending_manual_continue",
				Copies:         copies,
				PageCount:      printPageCount,
				Duplex:         true,
				DuplexHook:     hookURL,
				DuplexExpireAt: expiresAt,
				User:           user,
			})

			tracker := initJobStatusPoller(cfg)
			if tracker != nil {
				tracker.AddPendingJobWithStatus(initialJobID, printerID, "pending_manual_continue")
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
			log.Printf("[feishu-print] failed to detect document orientation, fallback to long-edge: %v", sideErr)
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

	filename := docFilename
	if filename == "" {
		filename = "feishu_document.pdf"
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
		persistPrintJobToBitable(c, cfg, printJobRecord{
			JobID:     jobID,
			PrinterID: printerID,
			FileName:  filename,
			Status:    "pending",
			Copies:    copies,
			PageCount: printPageCount,
			Duplex:    duplexMode != "off",
			User:      user,
		})

		tracker := initJobStatusPoller(cfg)
		if tracker != nil {
			tracker.AddPendingJob(jobID, printerID)
		}
	}
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "document"
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, "*", "_")
	name = strings.ReplaceAll(name, "?", "_")
	name = strings.ReplaceAll(name, "\"", "_")
	name = strings.ReplaceAll(name, "<", "_")
	name = strings.ReplaceAll(name, ">", "_")
	name = strings.ReplaceAll(name, "|", "_")
	name = strings.Trim(name, ". ")
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
