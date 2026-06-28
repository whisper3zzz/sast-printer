package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"goprint/api/pdfutil"
	"goprint/config"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-pdf/fpdf"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

type manualDuplexPending struct {
	JobID             string
	PrinterID         string
	RemainingFilePath string
	Copies            int
	PageCount         int
	OpenID            string
	CardID            string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ActionInProgress  bool
}

const (
	defaultManualDuplexMinTimeout     = 10 * time.Minute
	defaultManualDuplexPerPageTimeout = 30 * time.Second
	defaultManualDuplexExtendWindow   = 3 * time.Minute
)

var manualDuplexStore = struct {
	sync.RWMutex
	items map[string]manualDuplexPending
}{
	items: map[string]manualDuplexPending{},
}

func saveManualDuplexPending(jobID, printerID, remainingFilePath string, copies int, openID string, pageCount int) (string, time.Time, error) {
	token, err := randomToken(16)
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now()
	ttl := manualDuplexTimeoutForPrinterID(printerID, pageCount)
	expiresAt := now.Add(ttl)

	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()
	manualDuplexStore.items[token] = manualDuplexPending{
		JobID:             jobID,
		PrinterID:         printerID,
		RemainingFilePath: remainingFilePath,
		Copies:            copies,
		PageCount:         pageCount,
		OpenID:            openID,
		CreatedAt:         now,
		ExpiresAt:         expiresAt,
	}

	return token, expiresAt, nil
}

func getManualDuplexPending(token string) (manualDuplexPending, bool) {
	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()
	item, ok := manualDuplexStore.items[token]
	if !ok {
		return manualDuplexPending{}, false
	}

	if time.Now().After(item.ExpiresAt) {
		_ = os.Remove(item.RemainingFilePath)
		delete(manualDuplexStore.items, token)
		return manualDuplexPending{}, false
	}

	return item, ok
}

func claimManualDuplexPending(token string) (manualDuplexPending, bool) {
	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()

	item, ok := manualDuplexStore.items[token]
	if !ok {
		return manualDuplexPending{}, false
	}
	if time.Now().After(item.ExpiresAt) {
		_ = os.Remove(item.RemainingFilePath)
		delete(manualDuplexStore.items, token)
		return manualDuplexPending{}, false
	}
	if item.ActionInProgress {
		return manualDuplexPending{}, false
	}
	item.ActionInProgress = true
	manualDuplexStore.items[token] = item
	return item, true
}

func extendManualDuplexPending(token string) (manualDuplexPending, bool, error) {
	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()

	item, ok := manualDuplexStore.items[token]
	if !ok {
		return manualDuplexPending{}, false, nil
	}

	now := time.Now()
	if now.After(item.ExpiresAt) {
		_ = os.Remove(item.RemainingFilePath)
		delete(manualDuplexStore.items, token)
		return manualDuplexPending{}, false, nil
	}
	if item.ActionInProgress {
		return manualDuplexPending{}, false, nil
	}

	remaining := time.Until(item.ExpiresAt)
	window := manualDuplexExtendWindow()
	if remaining > window {
		return item, true, fmt.Errorf("manual duplex hook can only be extended within %s of expiration", window.Round(time.Second))
	}

	item.ExpiresAt = now.Add(manualDuplexMinTimeout())
	manualDuplexStore.items[token] = item
	return item, true, nil
}

func releaseManualDuplexPending(token string) {
	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()

	item, ok := manualDuplexStore.items[token]
	if !ok {
		return
	}
	item.ActionInProgress = false
	manualDuplexStore.items[token] = item
}

func deleteManualDuplexPending(token string) {
	manualDuplexStore.Lock()
	defer manualDuplexStore.Unlock()
	delete(manualDuplexStore.items, token)
}

func manualDuplexPendingByToken(token string) (manualDuplexPending, bool) {
	return getManualDuplexPending(token)
}

func prepareManualDuplexFiles(sourcePath string, printerCfg config.PrinterConfig, duplexEdge string) (string, string, func(), error) {
	// 根据 edge 类型选择配置
	var edgeConfig config.DuplexEdgeConfig
	switch duplexEdge {
	case "long-edge":
		edgeConfig = printerCfg.LongEdge
	case "short-edge":
		edgeConfig = printerCfg.ShortEdge
	default:
		return "", "", nil, fmt.Errorf("invalid duplex edge: %s", duplexEdge)
	}

	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to read pdf page count: %w", err)
	}
	if totalPages <= 0 {
		return "", "", nil, fmt.Errorf("invalid pdf page count: %d", totalPages)
	}

	workDir, err := os.MkdirTemp(tempDir(), "goprint-manual-duplex-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }

	firstPassPath := filepath.Join(workDir, "first-pass.pdf")
	secondPassFile, err := os.CreateTemp(tempDir(), "goprint-manual-duplex-second-pass-*.pdf")
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	secondPassPath := secondPassFile.Name()
	_ = secondPassFile.Close()

	workingSource := sourcePath
	padToEven := printerCfg.PadToEven != nil && *printerCfg.PadToEven
	if padToEven && totalPages%2 == 1 {
		blankTail := filepath.Join(workDir, "blank-tail.pdf")
		padded := filepath.Join(workDir, "padded-source.pdf")
		if err := createBlankPDF(blankTail, 1); err != nil {
			_ = os.Remove(secondPassPath)
			cleanup()
			return "", "", nil, err
		}
		if err := pdfapi.MergeCreateFile([]string{sourcePath, blankTail}, padded, false, nil); err != nil {
			_ = os.Remove(secondPassPath)
			cleanup()
			return "", "", nil, fmt.Errorf("failed to append blank tail page: %w", err)
		}
		workingSource = padded
		totalPages++
	}

	firstPassOdd := printerCfg.NormalizedFirstPass() == "odd"
	firstSelectors := pageSelectors(totalPages, !firstPassOdd)
	secondSelectors := pageSelectors(totalPages, firstPassOdd)

	if edgeConfig.ReverseFirstPass {
		reverseStrings(firstSelectors)
	}
	if edgeConfig.ReverseSecondPass {
		reverseStrings(secondSelectors)
	}

	if len(firstSelectors) == 0 || len(secondSelectors) == 0 {
		_ = os.Remove(secondPassPath)
		cleanup()
		return "", "", nil, fmt.Errorf("invalid page selectors generated for manual duplex")
	}

	if err := buildOrderedPDF(workingSource, firstPassPath, firstSelectors); err != nil {
		_ = os.Remove(secondPassPath)
		cleanup()
		return "", "", nil, fmt.Errorf("failed to build first pass pdf: %w", err)
	}

	if err := buildOrderedPDF(workingSource, secondPassPath, secondSelectors); err != nil {
		_ = os.Remove(secondPassPath)
		cleanup()
		return "", "", nil, fmt.Errorf("failed to build second pass pdf: %w", err)
	}

	if edgeConfig.RotateSecondPass {
		rotated := filepath.Join(workDir, "second-pass-rotated.pdf")
		if err := pdfapi.RotateFile(secondPassPath, rotated, 180, nil, nil); err != nil {
			_ = os.Remove(secondPassPath)
			cleanup()
			return "", "", nil, fmt.Errorf("failed to rotate second pass pdf: %w", err)
		}
		if err := os.Rename(rotated, secondPassPath); err != nil {
			_ = os.Remove(secondPassPath)
			cleanup()
			return "", "", nil, err
		}
	}

	return firstPassPath, secondPassPath, cleanup, nil
}

func pageSelectors(totalPages int, even bool) []string {
	selectors := make([]string, 0, totalPages/2+1)
	for i := 1; i <= totalPages; i++ {
		if even && i%2 == 0 {
			selectors = append(selectors, strconv.Itoa(i))
		}
		if !even && i%2 == 1 {
			selectors = append(selectors, strconv.Itoa(i))
		}
	}
	return selectors
}

func reverseStrings(items []string) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}

func createBlankPDF(path string, pages int) error {
	if pages <= 0 {
		return fmt.Errorf("invalid blank page count: %d", pages)
	}

	pdf := fpdf.New("P", "mm", "A4", "")
	for i := 0; i < pages; i++ {
		pdf.AddPage()
	}
	if err := pdf.OutputFileAndClose(path); err != nil {
		return fmt.Errorf("failed to create blank pdf: %w", err)
	}
	return nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func manualDuplexMinTimeout() time.Duration {
	cfg := getConfig()
	if cfg == nil {
		return defaultManualDuplexMinTimeout
	}

	raw := cfg.Printing.ManualDuplexMinTimeout
	if raw == "" {
		raw = cfg.Printing.ManualDuplexHookTTL
	}
	if raw == "" {
		return defaultManualDuplexMinTimeout
	}

	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return defaultManualDuplexMinTimeout
	}

	return timeout
}

func manualDuplexExtendWindow() time.Duration {
	cfg := getConfig()
	if cfg == nil {
		return defaultManualDuplexExtendWindow
	}

	window, err := time.ParseDuration(cfg.Printing.ManualDuplexExtendWindow)
	if err != nil || window <= 0 {
		return defaultManualDuplexExtendWindow
	}
	return window
}

func manualDuplexPerPageTimeout(printerCfg config.PrinterConfig) time.Duration {
	timeout, err := time.ParseDuration(printerCfg.ManualDuplexPerPageTimeout)
	if err != nil || timeout <= 0 {
		return defaultManualDuplexPerPageTimeout
	}
	return timeout
}

func manualDuplexTimeout(printerCfg config.PrinterConfig, pageCount int) time.Duration {
	minTimeout := manualDuplexMinTimeout()
	perPageTimeout := manualDuplexPerPageTimeout(printerCfg)

	timeout := minTimeout
	if pageCount > 0 {
		calculated := time.Duration(pageCount) * perPageTimeout
		if calculated > timeout {
			timeout = calculated
		}
	}

	return timeout
}

func manualDuplexTimeoutForPrinterID(printerID string, pageCount int) time.Duration {
	printerCfg, err := resolvePrinter(printerID)
	if err != nil {
		return manualDuplexMinTimeout()
	}
	return manualDuplexTimeout(printerCfg, pageCount)
}

func manualDuplexExtendWindowSeconds() int {
	return int(manualDuplexExtendWindow().Seconds())
}

func countPDFPages(sourcePath string) (int, error) {
	return pdfapi.PageCountFile(sourcePath)
}

func validatePDFPageLimit(cfg *config.Config, pages int) error {
	if pages <= 0 {
		return fmt.Errorf("invalid pdf page count: %d", pages)
	}
	if cfg != nil && cfg.Printing.MaxPDFPages > 0 && pages > cfg.Printing.MaxPDFPages {
		return fmt.Errorf("pdf has %d pages, exceeding configured limit of %d", pages, cfg.Printing.MaxPDFPages)
	}
	return nil
}

func enforcePDFPageLimit(cfg *config.Config, sourcePath string) (int, error) {
	pages, err := countPDFPages(sourcePath)
	if err != nil {
		return 0, fmt.Errorf("failed to read pdf page count: %w", err)
	}
	if err := validatePDFPageLimit(cfg, pages); err != nil {
		return 0, err
	}
	return pages, nil
}

func applyScalePercent(sourcePath string, scalePercent int) (string, func(), error) {
	if scalePercent <= 0 {
		scalePercent = 100
	}
	if scalePercent == 100 {
		return sourcePath, func() {}, nil
	}

	tmpFile, err := os.CreateTemp(tempDir(), "goprint-scale-*.pdf")
	if err != nil {
		return "", nil, err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	resize := &pdfmodel.Resize{Scale: float64(scalePercent) / 100.0}
	if err := pdfapi.ResizeFile(sourcePath, outPath, nil, resize, nil); err != nil {
		_ = os.Remove(outPath)
		return "", nil, fmt.Errorf("failed to scale pdf: %w", err)
	}

	return outPath, func() { _ = os.Remove(outPath) }, nil
}

func chooseAutoDuplexSides(sourcePath string) (string, error) {
	pageDims, err := pdfapi.PageDimsFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to read pdf page dimensions: %w", err)
	}
	if len(pageDims) == 0 {
		return "", fmt.Errorf("pdf has no pages")
	}

	landscapeCount := 0
	portraitCount := 0
	for _, dim := range pageDims {
		if dim.Width > dim.Height {
			landscapeCount++
		} else {
			portraitCount++
		}
	}

	if landscapeCount > portraitCount {
		return "two-sided-short-edge", nil
	}

	return "two-sided-long-edge", nil
}

func prepareReversedPDF(sourcePath string) (string, error) {
	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to read pdf page count: %w", err)
	}
	if totalPages <= 1 {
		return sourcePath, nil
	}

	selectors := make([]string, 0, totalPages)
	for i := totalPages; i >= 1; i-- {
		selectors = append(selectors, strconv.Itoa(i))
	}

	tmpFile, err := os.CreateTemp(tempDir(), "goprint-reverse-*.pdf")
	if err != nil {
		return "", err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	if err := buildOrderedPDF(sourcePath, outPath, selectors); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("failed to build reversed pdf: %w", err)
	}

	return outPath, nil
}

func ApplySingleSideReverse(sourcePath string) (string, error) {
	return prepareReversedPDF(sourcePath)
}

func buildOrderedPDF(sourcePath, outPath string, selectors []string) error {
	if len(selectors) == 0 {
		return fmt.Errorf("empty selectors")
	}

	workDir, err := os.MkdirTemp(tempDir(), "goprint-order-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	parts := make([]string, 0, len(selectors))
	for i, selector := range selectors {
		partPath := filepath.Join(workDir, fmt.Sprintf("part-%04d.pdf", i))
		if err := pdfapi.TrimFile(sourcePath, partPath, []string{selector}, nil); err != nil {
			return err
		}
		parts = append(parts, partPath)
	}

	if err := pdfapi.MergeCreateFile(parts, outPath, false, nil); err != nil {
		return err
	}
	return nil
}

func ApplyCollateCopies(sourcePath string, copies int) (string, error) {
	if copies <= 1 {
		return sourcePath, nil
	}

	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to read pdf page count: %w", err)
	}
	if totalPages <= 0 {
		return "", fmt.Errorf("invalid pdf page count: %d", totalPages)
	}

	selectors := make([]string, 0, totalPages*copies)
	for c := 0; c < copies; c++ {
		for p := 1; p <= totalPages; p++ {
			selectors = append(selectors, strconv.Itoa(p))
		}
	}

	tmpFile, err := os.CreateTemp(tempDir(), "goprint-collate-*.pdf")
	if err != nil {
		return "", err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	if err := buildOrderedPDF(sourcePath, outPath, selectors); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("failed to build collated pdf: %w", err)
	}

	return outPath, nil
}

func applyCopiesForPDF(sourcePath string, copies int, collate bool) (string, error) {
	if copies <= 1 {
		return sourcePath, nil
	}

	if collate {
		return ApplyCollateCopies(sourcePath, copies)
	}

	return ApplyUncollatedCopies(sourcePath, copies)
}

func ApplyUncollatedCopies(sourcePath string, copies int) (string, error) {
	if copies <= 1 {
		return sourcePath, nil
	}

	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to read pdf page count: %w", err)
	}
	if totalPages <= 0 {
		return "", fmt.Errorf("invalid pdf page count: %d", totalPages)
	}

	selectors := make([]string, 0, totalPages*copies)
	for p := 1; p <= totalPages; p++ {
		for c := 0; c < copies; c++ {
			selectors = append(selectors, strconv.Itoa(p))
		}
	}

	tmpFile, err := os.CreateTemp(tempDir(), "goprint-uncollate-*.pdf")
	if err != nil {
		return "", err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	if err := buildOrderedPDF(sourcePath, outPath, selectors); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("failed to build uncollated pdf: %w", err)
	}

	return outPath, nil
}

func BuildManualDuplexPreview(sourcePath string, printerCfg config.PrinterConfig, duplexEdge string, copies int, collate bool) (string, string, func(), error) {
	firstPassPath, secondPassPath, baseCleanup, err := prepareManualDuplexFiles(sourcePath, printerCfg, duplexEdge)
	if err != nil {
		return "", "", nil, err
	}

	firstPreviewPath, err := applyCopiesForPDF(firstPassPath, copies, collate)
	if err != nil {
		baseCleanup()
		_ = os.Remove(secondPassPath)
		return "", "", nil, err
	}

	secondPreviewPath, err := applyCopiesForPDF(secondPassPath, copies, collate)
	if err != nil {
		if firstPreviewPath != firstPassPath {
			_ = os.Remove(firstPreviewPath)
		}
		baseCleanup()
		_ = os.Remove(secondPassPath)
		return "", "", nil, err
	}

	cleanup := func() {
		if firstPreviewPath != firstPassPath {
			_ = os.Remove(firstPreviewPath)
		}
		if secondPreviewPath != secondPassPath {
			_ = os.Remove(secondPreviewPath)
		}
		_ = os.Remove(secondPassPath)
		baseCleanup()
	}

	return firstPreviewPath, secondPreviewPath, cleanup, nil
}

func SavePDFForTest(sourcePath, label string) (string, error) {
	if strings.TrimSpace(sourcePath) == "" {
		return "", fmt.Errorf("source path is empty")
	}

	if err := os.MkdirAll("test", 0o755); err != nil {
		return "", err
	}

	safeLabel := sanitizeLabel(label)
	if safeLabel == "" {
		safeLabel = "pdf"
	}

	fileName := fmt.Sprintf("%s-%s.pdf", time.Now().Format("20060102-150405"), safeLabel)
	outPath := filepath.Join("test", fileName)

	src, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}

	return outPath, nil
}

func sanitizeLabel(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		if r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('_')
	}
	return strings.Trim(b.String(), "_")
}

// extractPDF Pages 从 PDF 中提取指定页码的页面。
// pagesStr 格式支持："1,2,3" 或 "1-3,5,7-9" 等。
// 返回提取后 PDF 的文件路径，和一个清理函数。
// 调用者需要负责调用清理函数来删除临时文件。
func extractPDFPages(sourcePath string, pagesStr string) (string, func(), error) {
	if strings.TrimSpace(pagesStr) == "" {
		return sourcePath, func() {}, nil
	}

	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to count pdf pages: %w", err)
	}
	if totalPages <= 0 {
		return "", nil, fmt.Errorf("invalid pdf page count: %d", totalPages)
	}

	// 解析页码，构建 selector 列表
	selectors, err := parsePagesString(pagesStr, totalPages)
	if err != nil {
		return "", nil, fmt.Errorf("invalid pages format: %w", err)
	}

	if len(selectors) == 0 {
		return "", nil, fmt.Errorf("no valid pages specified")
	}

	// 创建临时文件存储提取后的 PDF
	tmpFile, err := os.CreateTemp(tempDir(), "goprint-pages-*.pdf")
	if err != nil {
		return "", nil, err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	// 使用 buildOrderedPDF 提取页面
	if err := buildOrderedPDF(sourcePath, outPath, selectors); err != nil {
		_ = os.Remove(outPath)
		return "", nil, fmt.Errorf("failed to extract pages: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(outPath)
	}

	return outPath, cleanup, nil
}

// parsePagesString 解析页码字符串
// 支持格式:
//   - "1,2,3" -> 页码 1, 2, 3
//   - "1-3" -> 页码 1, 2, 3
//   - "1-3,5,7-9" -> 页码 1, 2, 3, 5, 7, 8, 9
//   - 返回的是 pdfcpu selector 列表
func parsePagesString(pagesStr string, totalPages int) ([]string, error) {
	pagesStr = strings.TrimSpace(pagesStr)
	if pagesStr == "" {
		return []string{}, nil
	}

	pageMap := make(map[int]bool)
	parts := strings.Split(pagesStr, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 检查是否是范围 (e.g., "1-3")
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid range format: %s", part)
			}

			start, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid start page: %s", rangeParts[0])
			}

			end, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid end page: %s", rangeParts[1])
			}

			if start > end {
				start, end = end, start
			}

			if start < 1 || end < 1 || start > totalPages || end > totalPages {
				return nil, fmt.Errorf("page range %d-%d out of range (1-%d)", start, end, totalPages)
			}

			for i := start; i <= end; i++ {
				pageMap[i] = true
			}
		} else {
			// 单个页码
			page, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid page number: %s", part)
			}

			if page < 1 || page > totalPages {
				return nil, fmt.Errorf("page %d out of range (1-%d)", page, totalPages)
			}

			pageMap[page] = true
		}
	}

	// 转为排序的 selector 列表
	if len(pageMap) == 0 {
		return nil, fmt.Errorf("no valid pages extracted from: %s", pagesStr)
	}

	pageNums := make([]int, 0, len(pageMap))
	for page := range pageMap {
		pageNums = append(pageNums, page)
	}
	sort.Ints(pageNums)

	selectors := make([]string, 0, len(pageNums))
	for _, page := range pageNums {
		selectors = append(selectors, strconv.Itoa(page))
	}

	return selectors, nil
}

func applyNupLayout(sourcePath string, nup int, direction string) (string, func(), error) {
	if nup < 2 {
		return sourcePath, func() {}, nil
	}

	totalPages, err := pdfapi.PageCountFile(sourcePath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to count pdf pages: %w", err)
	}
	if totalPages <= 1 {
		return sourcePath, func() {}, nil
	}

	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction != "vertical" {
		direction = "horizontal"
	}
	return pdfutil.CreateNupPDF(sourcePath, nup, direction, nil, tempDir())
}
