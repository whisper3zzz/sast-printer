package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goprint/api/conversion"
	"goprint/config"
	"goprint/cups"

	"github.com/gin-gonic/gin"
)

var submitCupsJob = func(client *cups.CupsClient, printerName string, filePath string, opts cups.PrintOptions) (string, error) {
	return client.SubmitJob(printerName, filePath, opts)
}

var exportFeishuDocToPDFForPreview = func(ctx context.Context, cfg *config.Config, userToken string, rawURL string) (string, string, error) {
	client, err := newFeishuClient(cfg)
	if err != nil {
		return "", "", err
	}
	return exportFeishuDocToPDF(ctx, cfg, client, userToken, rawURL)
}

type submitFromPreviewRequest struct {
	PreviewID string `json:"preview_id"`
	PrinterID string `json:"printer_id"`
	RenderID  string `json:"render_id"`
	Pages     string `json:"pages"`
	Nup       int    `json:"nup"`
	Scale     int    `json:"scale"`
	Copies    int    `json:"copies"`
	Collate   *bool  `json:"collate"`
	Direction string `json:"direction"`
	Duplex    any    `json:"duplex"`
}

func CreateUploadPreviewArtifact(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	limitRequestBody(c, cfg)
	if !parseMultipartFormOrRespond(c, cfg) {
		return
	}
	defer cleanupMultipartForm(c)

	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}

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

	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "preview queue is busy or request cancelled",
			"error_code": "preview_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	defer releasePrintSubmitQueue()

	tempPath, cleanupUploaded, sourceHash, err := prepareUploadedSource(c, cfg, file, "goprint-preview-artifact")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to save uploaded file",
			"details": err.Error(),
		})
		return
	}
	defer cleanupUploaded()

	canonicalPath := tempPath
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
		canonicalPath = convertedPath
	}

	pageCount, err := enforcePDFPageLimit(cfg, canonicalPath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}

	artifact, err := newPreviewArtifactStore(cfg).saveCanonical(canonicalPath, "upload", file.Filename, access.OpenID, pageCount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to create preview artifact",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, previewArtifactResponse(artifact))
}

func CreateFeishuPreviewArtifact(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}
	userToken := extractUserAccessToken(c)
	if userToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing user access token"})
		return
	}

	var req feishuExportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body, expected {\"url\": \"...\"}"})
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "preview queue is busy or request cancelled",
			"error_code": "preview_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	defer releasePrintSubmitQueue()

	pdfPath, filename, err := exportFeishuDocToPDFForPreview(c.Request.Context(), cfg, userToken, req.URL)
	if err != nil {
		c.JSON(feishuExportHTTPStatus(err), gin.H{
			"error":      "failed to export feishu document",
			"error_code": "feishu_export_failed",
			"details":    err.Error(),
		})
		return
	}
	defer os.Remove(pdfPath)

	pageCount, err := enforcePDFPageLimit(cfg, pdfPath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "pdf page count exceeds configured limit or cannot be read",
			"error_code": "pdf_page_limit_exceeded",
			"details":    err.Error(),
		})
		return
	}
	if strings.TrimSpace(filename) == "" {
		filename = "feishu_document.pdf"
	} else {
		filename = sanitizeFilename(filename) + ".pdf"
	}

	artifact, err := newPreviewArtifactStore(cfg).saveCanonical(pdfPath, "feishu", filename, access.OpenID, pageCount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to create preview artifact",
			"details": err.Error(),
		})
		return
	}
	c.JSON(http.StatusCreated, previewArtifactResponse(artifact))
}

func DownloadPreviewArtifactFile(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}

	artifact, err := newPreviewArtifactStore(cfg).getArtifact(c.Param("preview_id"), access)
	if err != nil {
		respondPreviewArtifactError(c, err)
		return
	}

	name := strings.TrimSuffix(filepath.Base(artifact.Filename), filepath.Ext(artifact.Filename))
	if name == "" || name == "." {
		name = "document"
	}
	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", sanitizeFilename(name)+".pdf"))
	c.File(artifact.CanonicalPath)
}

func CreatePreviewArtifactRender(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}

	var raw previewRenderParamsInput
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	params, err := normalizePreviewRenderParams(cfg, raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	store := newPreviewArtifactStore(cfg)
	artifact, err := store.getArtifact(c.Param("preview_id"), access)
	if err != nil {
		respondPreviewArtifactError(c, err)
		return
	}

	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "preview queue is busy or request cancelled",
			"error_code": "preview_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	defer releasePrintSubmitQueue()

	render, err := store.ensureRender(artifact, params)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":   "failed to render preview artifact",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, previewRenderResponse(artifact, render))
}

func DownloadPreviewArtifactRenderFile(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}

	artifact, err := newPreviewArtifactStore(cfg).getArtifact(c.Param("preview_id"), access)
	if err != nil {
		respondPreviewArtifactError(c, err)
		return
	}
	render, ok := artifact.Renders[c.Param("render_id")]
	if !ok {
		respondPreviewArtifactError(c, errPreviewRenderNotFound)
		return
	}
	if info, err := os.Stat(render.Path); err != nil || info.IsDir() {
		respondPreviewArtifactError(c, errPreviewArtifactFileMissing)
		return
	}

	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", sanitizeFilename(strings.TrimSuffix(artifact.Filename, filepath.Ext(artifact.Filename)))+"-render.pdf"))
	c.File(render.Path)
}

func SubmitPrintJobFromPreview(c *gin.Context) {
	cfg, err := requireConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	access, ok := previewArtifactAccessFromContext(c, cfg)
	if !ok {
		return
	}

	var req submitFromPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	req.PreviewID = strings.TrimSpace(req.PreviewID)
	req.PrinterID = strings.TrimSpace(req.PrinterID)
	req.RenderID = strings.TrimSpace(req.RenderID)
	if req.PreviewID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "preview_id is required"})
		return
	}
	if req.PrinterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "printer_id is required"})
		return
	}

	params, err := normalizePreviewRenderParams(cfg, previewRenderParamsInput{
		Pages:     req.Pages,
		Nup:       req.Nup,
		Scale:     req.Scale,
		Copies:    req.Copies,
		Collate:   req.Collate,
		Direction: req.Direction,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	duplexRequested, duplexEdge, err := parseFromPreviewDuplex(req.Duplex)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	store := newPreviewArtifactStore(cfg)
	artifact, err := store.getArtifact(req.PreviewID, access)
	if err != nil {
		respondPreviewArtifactError(c, err)
		return
	}
	if req.RenderID != "" && !submitFromPreviewHasRenderParamOverrides(req) {
		if render, ok := artifact.Renders[req.RenderID]; ok {
			params = render.Params
		}
	}

	printerCfg, err := resolveVisiblePrinter(req.PrinterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error(), "printer_id": req.PrinterID})
		return
	}
	if duplexRequested && printerCfg.NormalizedDuplexMode() == "off" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "duplex requested but printer duplex_mode is off",
			"printer_id": req.PrinterID,
			"hint":       "set duplex_mode to auto or manual in config.yaml if this printer supports duplex",
		})
		return
	}

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "printer_id": req.PrinterID})
		return
	}

	if err := acquirePrintSubmitQueue(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "print queue is busy or request cancelled",
			"error_code": "print_queue_unavailable",
			"details":    err.Error(),
		})
		return
	}
	defer releasePrintSubmitQueue()

	basePath, baseCleanup, basePageCount, err := buildRenderedPDF(artifact.CanonicalPath, renderParamsWithoutCopies(params))
	if baseCleanup != nil {
		defer baseCleanup()
	}
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":   "failed to prepare preview artifact for printing",
			"details": err.Error(),
		})
		return
	}

	duplexMode := printerCfg.NormalizedDuplexMode()
	if !duplexRequested {
		duplexMode = "off"
	}
	if basePageCount == 1 {
		duplexMode = "off"
	}

	if duplexMode == "manual" {
		submitManualDuplexFromPreview(c, cfg, cupsClient, printerName, req, artifact, printerCfg, basePath, basePageCount, params, duplexEdge)
		return
	}

	printPath := ""
	cleanupPrintPath := func() {}
	if duplexMode == "off" && printerCfg.Reverse {
		reversedPath, reverseErr := prepareReversedPDF(basePath)
		if reverseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to reverse document for single-side printing",
				"details": reverseErr.Error(),
			})
			return
		}
		if reversedPath != basePath {
			defer os.Remove(reversedPath)
		}
		finalPath, copiesErr := applyCopiesMode(reversedPath, params.Copies, params.Collate)
		if copiesErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "failed to build copies",
				"details": copiesErr.Error(),
			})
			return
		}
		if finalPath != reversedPath {
			cleanupPrintPath = func() { _ = os.Remove(finalPath) }
		}
		printPath = finalPath
	} else {
		render, renderErr := renderForPreviewPrint(store, artifact, params, req.RenderID)
		if renderErr != nil {
			if errors.Is(renderErr, errPreviewArtifactFileMissing) || errors.Is(renderErr, errPreviewRenderNotFound) {
				respondPreviewArtifactError(c, renderErr)
				return
			}
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   "failed to render preview artifact",
				"details": renderErr.Error(),
			})
			return
		}
		printPath = render.Path
	}
	defer cleanupPrintPath()

	printOpts := cups.PrintOptions{Copies: 1, Collate: params.Collate}
	if duplexMode == "auto" {
		autoSides, sideErr := chooseAutoDuplexSides(printPath)
		if sideErr != nil {
			printOpts.Sides = "two-sided-long-edge"
		} else {
			printOpts.Sides = autoSides
		}
	}

	jobID, err := submitCupsJob(cupsClient, printerName, printPath, printOpts)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "failed to submit print job",
			"printer_id": req.PrinterID,
			"details":    err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"job_id":     jobID,
		"printer":    req.PrinterID,
		"copies":     params.Copies,
		"collate":    params.Collate,
		"status":     "pending",
		"duplex":     duplexMode != "off",
		"note":       printerCfg.Note,
		"message":    "Print job submitted successfully",
		"preview_id": artifact.ID,
	})

	if user, ok := currentAuthUser(c); ok {
		persistPrintJobToBitable(c, cfg, printJobRecord{
			JobID:     jobID,
			PrinterID: req.PrinterID,
			FileName:  artifact.Filename,
			Status:    "pending",
			Copies:    params.Copies,
			PageCount: basePageCount,
			Duplex:    duplexMode != "off",
			User:      user,
		})

		tracker := initJobStatusPoller(cfg)
		if tracker != nil {
			tracker.AddPendingJob(jobID, req.PrinterID)
		}
	}
}

func submitManualDuplexFromPreview(c *gin.Context, cfg *config.Config, cupsClient *cups.CupsClient, printerName string, req submitFromPreviewRequest, artifact *previewArtifact, printerCfg config.PrinterConfig, basePath string, basePageCount int, params previewRenderParams, duplexEdge string) {
	firstPassPath, secondPassPath, cleanup, err := prepareManualDuplexFiles(basePath, printerCfg, duplexEdge)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "failed to prepare manual duplex files",
			"details": err.Error(),
		})
		return
	}
	defer cleanup()

	firstPassToSubmit, err := applyCopiesMode(firstPassPath, params.Copies, params.Collate)
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

	secondPassToStore, err := applyCopiesMode(secondPassPath, params.Copies, params.Collate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "failed to build second pass copies",
			"details": err.Error(),
		})
		return
	}

	initialJobID, err := submitCupsJob(cupsClient, printerName, firstPassToSubmit, cups.PrintOptions{Copies: 1})
	if err != nil {
		_ = os.Remove(secondPassToStore)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "failed to submit first pass for manual duplex",
			"printer_id": req.PrinterID,
			"details":    err.Error(),
		})
		return
	}

	openID := ""
	if user, ok := currentAuthUser(c); ok {
		openID = user.OpenID
	}
	token, expiresAt, err := saveManualDuplexPending(initialJobID, req.PrinterID, secondPassToStore, 1, openID, basePageCount*params.Copies)
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
		"printer":                    req.PrinterID,
		"copies":                     params.Copies,
		"collate":                    params.Collate,
		"status":                     "pending",
		"duplex":                     true,
		"note":                       printerCfg.Note,
		"message":                    "First pass submitted. Use hook_url to print remaining pages.",
		"hook_url":                   hookURL,
		"hook_expires_at":            expiresAt.In(time.Local).Format("2006-01-02 15:04"),
		"hook_extend_window_seconds": manualDuplexExtendWindowSeconds(),
		"preview_id":                 artifact.ID,
	})

	if user, ok := currentAuthUser(c); ok {
		persistPrintJobToBitable(c, cfg, printJobRecord{
			JobID:          initialJobID,
			PrinterID:      req.PrinterID,
			FileName:       artifact.Filename,
			Status:         "pending_manual_continue",
			Copies:         params.Copies,
			PageCount:      basePageCount,
			Duplex:         true,
			DuplexHook:     hookURL,
			DuplexExpireAt: expiresAt,
			User:           user,
		})

		tracker := initJobStatusPoller(cfg)
		if tracker != nil {
			tracker.AddPendingJobWithStatus(initialJobID, req.PrinterID, "pending_manual_continue")
		}
	}
}

func renderForPreviewPrint(store *previewArtifactStore, artifact *previewArtifact, params previewRenderParams, renderID string) (*previewRenderArtifact, error) {
	if strings.TrimSpace(renderID) == "" {
		return store.ensureRender(artifact, params)
	}
	render, ok := artifact.Renders[renderID]
	if !ok {
		return nil, errPreviewRenderNotFound
	}
	if render.Params != params {
		return nil, fmt.Errorf("render_id does not match requested print parameters")
	}
	if info, err := os.Stat(render.Path); err != nil || info.IsDir() {
		return nil, errPreviewArtifactFileMissing
	}
	return &render, nil
}

func submitFromPreviewHasRenderParamOverrides(req submitFromPreviewRequest) bool {
	return strings.TrimSpace(req.Pages) != "" ||
		req.Nup != 0 ||
		req.Scale != 0 ||
		req.Copies != 0 ||
		req.Collate != nil ||
		strings.TrimSpace(req.Direction) != ""
}

func parseFromPreviewDuplex(raw any) (bool, string, error) {
	if raw == nil {
		return false, "long-edge", nil
	}
	switch v := raw.(type) {
	case bool:
		if v {
			return true, "long-edge", nil
		}
		return false, "long-edge", nil
	case string:
		value := strings.TrimSpace(strings.ToLower(v))
		switch value {
		case "", "0", "false", "no", "off":
			return false, "long-edge", nil
		case "1", "true", "yes", "on":
			return true, "long-edge", nil
		case "long-edge", "two-sided-long-edge":
			return true, "long-edge", nil
		case "short-edge", "two-sided-short-edge":
			return true, "short-edge", nil
		default:
			return false, "", fmt.Errorf("duplex must be one of: true/false/1/0/yes/no/on/off/long-edge/short-edge")
		}
	case float64:
		if v == 0 {
			return false, "long-edge", nil
		}
		if v == 1 {
			return true, "long-edge", nil
		}
	}
	return false, "", fmt.Errorf("duplex must be one of: true/false/1/0/yes/no/on/off/long-edge/short-edge")
}

func previewArtifactAccessFromContext(c *gin.Context, cfg *config.Config) (previewArtifactAccess, bool) {
	if cfg == nil || !cfg.Auth.Enabled {
		return previewArtifactAccess{}, true
	}
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authenticated user context"})
		return previewArtifactAccess{}, false
	}
	return previewArtifactAccess{AuthEnabled: true, OpenID: user.OpenID}, true
}

func previewArtifactResponse(artifact *previewArtifact) gin.H {
	return gin.H{
		"preview_id":         artifact.ID,
		"source_type":        artifact.SourceType,
		"filename":           artifact.Filename,
		"page_count":         artifact.PageCount,
		"expires_at":         artifact.ExpiresAt.Format(time.RFC3339),
		"canonical_file_url": "/api/jobs/previews/" + artifact.ID + "/file",
	}
}

func previewRenderResponse(artifact *previewArtifact, render *previewRenderArtifact) gin.H {
	return gin.H{
		"preview_id":  artifact.ID,
		"render_id":   render.ID,
		"page_count":  render.PageCount,
		"file_url":    "/api/jobs/previews/" + artifact.ID + "/renders/" + render.ID + "/file",
		"expires_at":  artifact.ExpiresAt.Format(time.RFC3339),
		"parameters":  render.Params,
		"source_type": artifact.SourceType,
	}
}

func respondPreviewArtifactError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errPreviewArtifactForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "preview artifact access denied", "error_code": "preview_artifact_forbidden"})
	case errors.Is(err, errPreviewArtifactFileMissing):
		c.JSON(http.StatusGone, gin.H{"error": "preview artifact file is missing", "error_code": "preview_artifact_file_missing"})
	case errors.Is(err, errPreviewRenderNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "preview render not found", "error_code": "preview_render_not_found"})
	case errors.Is(err, errPreviewArtifactNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "preview artifact not found", "error_code": "preview_artifact_not_found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "preview artifact error", "details": err.Error()})
	}
}
