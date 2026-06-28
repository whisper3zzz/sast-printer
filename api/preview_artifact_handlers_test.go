package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goprint/config"
	"goprint/cups"

	"github.com/gin-gonic/gin"
)

func TestCreateUploadPreviewArtifactReturnsPreviewID(t *testing.T) {
	router, cfg := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	resp := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}

	var body struct {
		PreviewID        string `json:"preview_id"`
		SourceType       string `json:"source_type"`
		Filename         string `json:"filename"`
		PageCount        int    `json:"page_count"`
		CanonicalFileURL string `json:"canonical_file_url"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.PreviewID == "" {
		t.Fatal("preview_id is empty")
	}
	if body.SourceType != "upload" || body.Filename != "source.pdf" || body.PageCount != 1 {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.CanonicalFileURL != "/api/jobs/previews/"+body.PreviewID+"/file" {
		t.Fatalf("canonical_file_url = %q", body.CanonicalFileURL)
	}
	if _, err := os.Stat(filepath.Join(cfg.Printing.TempDir, previewArtifactsDirName, body.PreviewID, "canonical.pdf")); err != nil {
		t.Fatalf("canonical artifact missing: %v", err)
	}
}

func TestDownloadPreviewArtifactFileReturnsPDF(t *testing.T) {
	router, _ := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/previews/"+createBody.PreviewID+"/file", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", got)
	}
	if !bytes.HasPrefix(resp.Body.Bytes(), []byte("%PDF")) {
		t.Fatalf("download body does not look like PDF: %q", resp.Body.String()[:min(20, resp.Body.Len())])
	}
}

func TestRenderPreviewArtifactReturnsStableRenderID(t *testing.T) {
	router, _ := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	first := performJSON(t, router, http.MethodPost, "/api/jobs/previews/"+createBody.PreviewID+"/renders", map[string]any{"copies": 2})
	second := performJSON(t, router, http.MethodPost, "/api/jobs/previews/"+createBody.PreviewID+"/renders", map[string]any{"copies": 2})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d, body=%s", first.Code, http.StatusOK, first.Body.String())
	}
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d, body=%s", second.Code, http.StatusOK, second.Body.String())
	}

	var firstBody, secondBody struct {
		RenderID  string `json:"render_id"`
		PageCount int    `json:"page_count"`
		FileURL   string `json:"file_url"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if firstBody.RenderID == "" {
		t.Fatal("render_id is empty")
	}
	if firstBody.RenderID != secondBody.RenderID {
		t.Fatalf("render IDs differ: %s vs %s", firstBody.RenderID, secondBody.RenderID)
	}
	if firstBody.PageCount != 2 {
		t.Fatalf("PageCount = %d, want 2", firstBody.PageCount)
	}
	if firstBody.FileURL != "/api/jobs/previews/"+createBody.PreviewID+"/renders/"+firstBody.RenderID+"/file" {
		t.Fatalf("file_url = %q", firstBody.FileURL)
	}
}

func TestPreviewArtifactOwnerMismatchReturnsForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := testPreviewArtifactConfig(t)
	cfg.Auth.Enabled = true
	prevCfg := getConfig()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(sourcePath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}
	store := newPreviewArtifactStore(cfg)
	artifact, err := store.saveCanonical(sourcePath, "upload", "source.pdf", "ou_owner", 1)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth_user", feishuUserInfo{OpenID: "ou_other"})
		c.Next()
	})
	router.GET("/api/jobs/previews/:preview_id/file", DownloadPreviewArtifactFile)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/previews/"+artifact.ID+"/file", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
}

func TestCreateFeishuPreviewArtifactReturnsPreviewID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := testPreviewArtifactConfig(t)
	prevCfg := getConfig()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })

	restore := replaceExportFeishuDocToPDFForPreviewForTest(func(_ context.Context, cfg *config.Config, userToken string, rawURL string) (string, string, error) {
		if userToken != "user-token" {
			t.Fatalf("userToken = %q, want user-token", userToken)
		}
		if rawURL == "" {
			t.Fatal("rawURL is empty")
		}
		outPath := filepath.Join(cfg.Printing.TempDir, "feishu.pdf")
		if err := createBlankPDF(outPath, 1); err != nil {
			return "", "", err
		}
		return outPath, "Feishu Doc", nil
	})
	t.Cleanup(restore)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth_token", "user-token")
		c.Next()
	})
	router.POST("/api/jobs/previews/feishu", CreateFeishuPreviewArtifact)

	resp := performJSON(t, router, http.MethodPost, "/api/jobs/previews/feishu", map[string]any{
		"url": "https://example.feishu.cn/docx/doc-token",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}

	var body struct {
		PreviewID  string `json:"preview_id"`
		SourceType string `json:"source_type"`
		Filename   string `json:"filename"`
		PageCount  int    `json:"page_count"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.PreviewID == "" || body.SourceType != "feishu" || body.Filename != "Feishu Doc.pdf" || body.PageCount != 1 {
		t.Fatalf("unexpected response: %+v", body)
	}
	if _, err := os.Stat(filepath.Join(cfg.Printing.TempDir, previewArtifactsDirName, body.PreviewID, "canonical.pdf")); err != nil {
		t.Fatalf("canonical artifact missing: %v", err)
	}
}

func TestSubmitPrintJobFromPreviewUsesExistingRender(t *testing.T) {
	router, cfg := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	rendered := performJSON(t, router, http.MethodPost, "/api/jobs/previews/"+createBody.PreviewID+"/renders", map[string]any{"copies": 2})
	var renderBody struct {
		RenderID string `json:"render_id"`
	}
	if err := json.Unmarshal(rendered.Body.Bytes(), &renderBody); err != nil {
		t.Fatalf("decode render response: %v", err)
	}
	store := newPreviewArtifactStore(cfg)
	artifact, err := store.getArtifact(createBody.PreviewID, previewArtifactAccess{})
	if err != nil {
		t.Fatalf("getArtifact: %v", err)
	}
	wantPath := artifact.Renders[renderBody.RenderID].Path

	var submittedPath string
	var submittedOpts cups.PrintOptions
	restore := replaceSubmitCupsJobForTest(func(_ *cups.CupsClient, printerName string, filePath string, opts cups.PrintOptions) (string, error) {
		if printerName != "printer-1" {
			t.Fatalf("printerName = %q, want printer-1", printerName)
		}
		submittedPath = filePath
		submittedOpts = opts
		return "job-123", nil
	})
	t.Cleanup(restore)

	resp := performJSON(t, router, http.MethodPost, "/api/jobs/from-preview", map[string]any{
		"preview_id": createBody.PreviewID,
		"printer_id": "printer-1",
		"copies":     2,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	if submittedPath != wantPath {
		t.Fatalf("submitted path = %q, want existing render %q", submittedPath, wantPath)
	}
	if submittedOpts.Copies != 1 {
		t.Fatalf("submitted Copies = %d, want 1", submittedOpts.Copies)
	}
	var body struct {
		JobID  string `json:"job_id"`
		Copies int    `json:"copies"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.JobID != "job-123" || body.Copies != 2 {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestSubmitPrintJobFromPreviewUsesRenderIDWithoutRepeatingParams(t *testing.T) {
	router, cfg := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	rendered := performJSON(t, router, http.MethodPost, "/api/jobs/previews/"+createBody.PreviewID+"/renders", map[string]any{"copies": 2})
	var renderBody struct {
		RenderID string `json:"render_id"`
	}
	if err := json.Unmarshal(rendered.Body.Bytes(), &renderBody); err != nil {
		t.Fatalf("decode render response: %v", err)
	}
	artifact, err := newPreviewArtifactStore(cfg).getArtifact(createBody.PreviewID, previewArtifactAccess{})
	if err != nil {
		t.Fatalf("getArtifact: %v", err)
	}
	wantPath := artifact.Renders[renderBody.RenderID].Path

	var submittedPath string
	restore := replaceSubmitCupsJobForTest(func(_ *cups.CupsClient, _ string, filePath string, _ cups.PrintOptions) (string, error) {
		submittedPath = filePath
		return "job-234", nil
	})
	t.Cleanup(restore)

	resp := performJSON(t, router, http.MethodPost, "/api/jobs/from-preview", map[string]any{
		"preview_id": createBody.PreviewID,
		"printer_id": "printer-1",
		"render_id":  renderBody.RenderID,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	if submittedPath != wantPath {
		t.Fatalf("submitted path = %q, want render path %q", submittedPath, wantPath)
	}
	var body struct {
		Copies int `json:"copies"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Copies != 2 {
		t.Fatalf("copies = %d, want render copies 2", body.Copies)
	}
}

func TestSubmitPrintJobFromPreviewGeneratesMissingRender(t *testing.T) {
	router, cfg := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	var submittedPath string
	restore := replaceSubmitCupsJobForTest(func(_ *cups.CupsClient, _ string, filePath string, _ cups.PrintOptions) (string, error) {
		submittedPath = filePath
		return "job-456", nil
	})
	t.Cleanup(restore)

	resp := performJSON(t, router, http.MethodPost, "/api/jobs/from-preview", map[string]any{
		"preview_id": createBody.PreviewID,
		"printer_id": "printer-1",
		"copies":     2,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	if submittedPath == "" {
		t.Fatal("submit was not called")
	}
	if !strings.Contains(submittedPath, filepath.Join(previewArtifactsDirName, createBody.PreviewID, "renders")) {
		t.Fatalf("submitted path = %q, want generated render artifact", submittedPath)
	}

	artifact, err := newPreviewArtifactStore(cfg).getArtifact(createBody.PreviewID, previewArtifactAccess{})
	if err != nil {
		t.Fatalf("getArtifact: %v", err)
	}
	if len(artifact.Renders) != 1 {
		t.Fatalf("render count = %d, want 1", len(artifact.Renders))
	}
}

func TestSubmitPrintJobFromPreviewReturnsNotFoundForExpiredArtifact(t *testing.T) {
	router, cfg := setupPreviewArtifactTestRouter(t)
	pdfPath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(pdfPath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	created := performMultipartUpload(t, router, "/api/jobs/previews", pdfPath, "source.pdf")
	var createBody struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	store := newPreviewArtifactStore(cfg)
	artifact, err := store.getArtifact(createBody.PreviewID, previewArtifactAccess{})
	if err != nil {
		t.Fatalf("getArtifact: %v", err)
	}
	artifact.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.writeArtifactMetadata(artifact); err != nil {
		t.Fatalf("writeArtifactMetadata: %v", err)
	}

	resp := performJSON(t, router, http.MethodPost, "/api/jobs/from-preview", map[string]any{
		"preview_id": createBody.PreviewID,
		"printer_id": "printer-1",
	})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func setupPreviewArtifactTestRouter(t *testing.T) (*gin.Engine, *config.Config) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetIPRateLimiterForTest()
	t.Cleanup(resetIPRateLimiterForTest)
	cfg := testPreviewArtifactConfig(t)
	cfg.Auth.Enabled = false
	cfg.SaneAPI.AuthEnabled = boolPtr(false)
	prevCfg := getConfig()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })
	return SetupRouter(), cfg
}

func performMultipartUpload(t *testing.T, router http.Handler, path string, filePath string, filename string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func performJSON(t *testing.T, router http.Handler, method string, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func boolPtr(v bool) *bool {
	return &v
}

func replaceSubmitCupsJobForTest(fn func(*cups.CupsClient, string, string, cups.PrintOptions) (string, error)) func() {
	prev := submitCupsJob
	submitCupsJob = fn
	return func() {
		submitCupsJob = prev
	}
}

func replaceExportFeishuDocToPDFForPreviewForTest(fn func(context.Context, *config.Config, string, string) (string, string, error)) func() {
	prev := exportFeishuDocToPDFForPreview
	exportFeishuDocToPDFForPreview = fn
	return func() {
		exportFeishuDocToPDFForPreview = prev
	}
}
