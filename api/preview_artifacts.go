package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"goprint/config"
)

const previewArtifactsDirName = "preview-artifacts"

var (
	errPreviewArtifactNotFound    = errors.New("preview artifact not found")
	errPreviewArtifactForbidden   = errors.New("preview artifact forbidden")
	errPreviewArtifactFileMissing = errors.New("preview artifact file missing")
	errPreviewRenderNotFound      = errors.New("preview render not found")
)

type previewArtifactAccess struct {
	AuthEnabled bool
	OpenID      string
}

type previewRenderParams struct {
	Pages     string `json:"pages"`
	Nup       int    `json:"nup"`
	Scale     int    `json:"scale"`
	Copies    int    `json:"copies"`
	Collate   bool   `json:"collate"`
	Direction string `json:"direction"`
}

type previewRenderArtifact struct {
	ID        string              `json:"id"`
	Params    previewRenderParams `json:"params"`
	Path      string              `json:"path"`
	PageCount int                 `json:"page_count"`
	CreatedAt time.Time           `json:"created_at"`
}

type previewArtifact struct {
	ID            string                           `json:"id"`
	OwnerOpenID   string                           `json:"owner_open_id,omitempty"`
	SourceType    string                           `json:"source_type"`
	Filename      string                           `json:"filename"`
	CanonicalPath string                           `json:"canonical_path"`
	PageCount     int                              `json:"page_count"`
	CreatedAt     time.Time                        `json:"created_at"`
	ExpiresAt     time.Time                        `json:"expires_at"`
	Renders       map[string]previewRenderArtifact `json:"renders,omitempty"`
}

type previewRenderBuilder func(sourcePath string, params previewRenderParams) (string, func(), int, error)

type previewArtifactStore struct {
	cfg           *config.Config
	root          string
	now           func() time.Time
	renderBuilder previewRenderBuilder
	mu            sync.Mutex
}

func newPreviewArtifactStore(cfg *config.Config) *previewArtifactStore {
	return &previewArtifactStore{
		cfg:           cfg,
		root:          previewArtifactsRoot(cfg),
		now:           time.Now,
		renderBuilder: buildRenderedPDF,
	}
}

func previewArtifactTTL(cfg *config.Config) time.Duration {
	const fallback = 30 * time.Minute
	if cfg == nil {
		return fallback
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(cfg.Printing.PreviewArtifactTTL))
	if err != nil || ttl <= 0 {
		return fallback
	}
	return ttl
}

func previewArtifactsRoot(cfg *config.Config) string {
	dir := ""
	if cfg != nil {
		dir = strings.TrimSpace(cfg.Printing.TempDir)
	}
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, previewArtifactsDirName)
}

func (s *previewArtifactStore) saveCanonical(sourcePath, sourceType, filename, ownerOpenID string, pageCount int) (*previewArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(sourcePath) == "" {
		return nil, fmt.Errorf("source path is empty")
	}
	if pageCount <= 0 {
		return nil, fmt.Errorf("invalid page count: %d", pageCount)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create preview artifact root: %w", err)
	}

	var artifactDir string
	var id string
	for i := 0; i < 5; i++ {
		generated, err := randomToken(16)
		if err != nil {
			return nil, err
		}
		id = generated
		artifactDir = filepath.Join(s.root, id)
		if _, err := os.Stat(artifactDir); os.IsNotExist(err) {
			break
		}
		if i == 4 {
			return nil, fmt.Errorf("failed to allocate unique preview artifact id")
		}
	}

	if err := os.MkdirAll(filepath.Join(artifactDir, "renders"), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create preview artifact dir: %w", err)
	}

	canonicalPath := filepath.Join(artifactDir, "canonical.pdf")
	if err := copyFileAtomic(sourcePath, canonicalPath); err != nil {
		_ = os.RemoveAll(artifactDir)
		return nil, fmt.Errorf("failed to store canonical pdf: %w", err)
	}

	now := s.now().UTC()
	artifact := &previewArtifact{
		ID:            id,
		OwnerOpenID:   strings.TrimSpace(ownerOpenID),
		SourceType:    strings.TrimSpace(sourceType),
		Filename:      strings.TrimSpace(filename),
		CanonicalPath: canonicalPath,
		PageCount:     pageCount,
		CreatedAt:     now,
		ExpiresAt:     now.Add(previewArtifactTTL(s.cfg)),
		Renders:       map[string]previewRenderArtifact{},
	}
	if artifact.SourceType == "" {
		artifact.SourceType = "upload"
	}
	if artifact.Filename == "" {
		artifact.Filename = "document.pdf"
	}

	if err := s.writeArtifactMetadata(artifact); err != nil {
		_ = os.RemoveAll(artifactDir)
		return nil, err
	}

	return artifact, nil
}

func (s *previewArtifactStore) getArtifact(id string, access previewArtifactAccess) (*previewArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	artifact, err := s.readArtifact(id)
	if err != nil {
		return nil, err
	}
	if !s.now().Before(artifact.ExpiresAt) {
		_ = os.RemoveAll(filepath.Join(s.root, artifact.ID))
		return nil, errPreviewArtifactNotFound
	}
	if access.AuthEnabled {
		if strings.TrimSpace(access.OpenID) == "" || artifact.OwnerOpenID != access.OpenID {
			return nil, errPreviewArtifactForbidden
		}
	}
	if info, err := os.Stat(artifact.CanonicalPath); err != nil || info.IsDir() {
		return nil, errPreviewArtifactFileMissing
	}
	if artifact.Renders == nil {
		artifact.Renders = map[string]previewRenderArtifact{}
	}
	return artifact, nil
}

func (s *previewArtifactStore) ensureRender(artifact *previewArtifact, params previewRenderParams) (*previewRenderArtifact, error) {
	if artifact == nil {
		return nil, errPreviewArtifactNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fresh, err := s.readArtifact(artifact.ID)
	if err != nil {
		return nil, err
	}
	if !s.now().Before(fresh.ExpiresAt) {
		_ = os.RemoveAll(filepath.Join(s.root, fresh.ID))
		return nil, errPreviewArtifactNotFound
	}
	if fresh.Renders == nil {
		fresh.Renders = map[string]previewRenderArtifact{}
	}

	renderID, err := previewRenderID(params)
	if err != nil {
		return nil, err
	}
	if existing, ok := fresh.Renders[renderID]; ok {
		if info, err := os.Stat(existing.Path); err == nil && !info.IsDir() {
			*artifact = *fresh
			return &existing, nil
		}
		delete(fresh.Renders, renderID)
	}

	builder := s.renderBuilder
	if builder == nil {
		return nil, fmt.Errorf("preview render builder is not configured")
	}

	renderedPath, cleanup, pageCount, err := builder(fresh.CanonicalPath, params)
	if cleanup == nil {
		cleanup = func() {}
	}
	defer cleanup()
	if err != nil {
		return nil, err
	}
	if pageCount <= 0 {
		return nil, fmt.Errorf("invalid rendered page count: %d", pageCount)
	}

	renderDir := filepath.Join(s.root, fresh.ID, "renders")
	if err := os.MkdirAll(renderDir, 0o755); err != nil {
		return nil, err
	}
	storedPath := filepath.Join(renderDir, renderID+".pdf")
	if err := copyFileAtomic(renderedPath, storedPath); err != nil {
		return nil, fmt.Errorf("failed to store rendered pdf: %w", err)
	}

	render := previewRenderArtifact{
		ID:        renderID,
		Params:    params,
		Path:      storedPath,
		PageCount: pageCount,
		CreatedAt: s.now().UTC(),
	}
	fresh.Renders[renderID] = render
	if err := s.writeArtifactMetadata(fresh); err != nil {
		_ = os.Remove(storedPath)
		return nil, err
	}
	*artifact = *fresh
	return &render, nil
}

func (s *previewArtifactStore) cleanupExpired() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		artifact, err := s.readArtifact(entry.Name())
		if err != nil {
			continue
		}
		if s.now().Before(artifact.ExpiresAt) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *previewArtifactStore) readArtifact(id string) (*previewArtifact, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, string(filepath.Separator)) {
		return nil, errPreviewArtifactNotFound
	}

	data, err := os.ReadFile(filepath.Join(s.root, id, "meta.json"))
	if os.IsNotExist(err) {
		return nil, errPreviewArtifactNotFound
	}
	if err != nil {
		return nil, err
	}

	var artifact previewArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, err
	}
	if artifact.ID == "" {
		artifact.ID = id
	}
	if artifact.ID != id {
		return nil, errPreviewArtifactNotFound
	}
	if artifact.Renders == nil {
		artifact.Renders = map[string]previewRenderArtifact{}
	}
	return &artifact, nil
}

func (s *previewArtifactStore) writeArtifactMetadata(artifact *previewArtifact) error {
	if artifact == nil {
		return errPreviewArtifactNotFound
	}
	artifactDir := filepath.Join(s.root, artifact.ID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(artifactDir, "meta-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(artifactDir, "meta.json")); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func previewRenderID(params previewRenderParams) (string, error) {
	normalized := previewRenderParams{
		Pages:     strings.TrimSpace(params.Pages),
		Nup:       params.Nup,
		Scale:     params.Scale,
		Copies:    params.Copies,
		Collate:   params.Collate,
		Direction: strings.ToLower(strings.TrimSpace(params.Direction)),
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func copyFileAtomic(sourcePath, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return err
	}
	removeTmp = false
	return nil
}
