package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goprint/config"
)

func TestPreviewArtifactStoreSaveCanonicalWritesMetadata(t *testing.T) {
	cfg := testPreviewArtifactConfig(t)
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	store := newPreviewArtifactStore(cfg)
	store.now = func() time.Time { return now }

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(sourcePath, []byte("%PDF-1.7\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	artifact, err := store.saveCanonical(sourcePath, "upload", "source.pdf", "ou_123", 3)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	if artifact.ID == "" {
		t.Fatal("artifact ID is empty")
	}
	if artifact.OwnerOpenID != "ou_123" {
		t.Fatalf("OwnerOpenID = %q, want ou_123", artifact.OwnerOpenID)
	}
	if artifact.PageCount != 3 {
		t.Fatalf("PageCount = %d, want 3", artifact.PageCount)
	}
	if got, want := artifact.ExpiresAt, now.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(store.root, artifact.ID, "meta.json")); err != nil {
		t.Fatalf("stat meta.json: %v", err)
	}
	if data, err := os.ReadFile(artifact.CanonicalPath); err != nil {
		t.Fatalf("read canonical: %v", err)
	} else if string(data) != "%PDF-1.7\n" {
		t.Fatalf("canonical data = %q", string(data))
	}
}

func TestPreviewArtifactStoreRejectsExpiredArtifact(t *testing.T) {
	cfg := testPreviewArtifactConfig(t)
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	store := newPreviewArtifactStore(cfg)
	store.now = func() time.Time { return now }

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(sourcePath, []byte("%PDF-1.7\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	artifact, err := store.saveCanonical(sourcePath, "upload", "source.pdf", "", 1)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	store.now = func() time.Time { return now.Add(31 * time.Minute) }
	_, err = store.getArtifact(artifact.ID, previewArtifactAccess{})
	if !errors.Is(err, errPreviewArtifactNotFound) {
		t.Fatalf("getArtifact err = %v, want errPreviewArtifactNotFound", err)
	}
	if _, statErr := os.Stat(filepath.Join(store.root, artifact.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("expired artifact dir still exists or stat failed differently: %v", statErr)
	}
}

func TestPreviewArtifactStoreCleanupExpiredPreservesActive(t *testing.T) {
	cfg := testPreviewArtifactConfig(t)
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	store := newPreviewArtifactStore(cfg)
	store.now = func() time.Time { return now }

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(sourcePath, []byte("%PDF-1.7\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	active, err := store.saveCanonical(sourcePath, "upload", "active.pdf", "", 1)
	if err != nil {
		t.Fatalf("save active: %v", err)
	}
	expired, err := store.saveCanonical(sourcePath, "upload", "expired.pdf", "", 1)
	if err != nil {
		t.Fatalf("save expired: %v", err)
	}
	expired.ExpiresAt = now.Add(-time.Minute)
	if err := store.writeArtifactMetadata(expired); err != nil {
		t.Fatalf("write expired metadata: %v", err)
	}

	removed, err := store.cleanupExpired()
	if err != nil {
		t.Fatalf("cleanupExpired: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(store.root, active.ID)); err != nil {
		t.Fatalf("active artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, expired.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired artifact still exists or stat failed differently: %v", err)
	}
}

func TestPreviewArtifactStoreEnsureRenderReusesSameParams(t *testing.T) {
	cfg := testPreviewArtifactConfig(t)
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	store := newPreviewArtifactStore(cfg)
	store.now = func() time.Time { return now }

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(sourcePath, []byte("%PDF-1.7\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	artifact, err := store.saveCanonical(sourcePath, "upload", "source.pdf", "", 1)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	builds := 0
	store.renderBuilder = func(sourcePath string, params previewRenderParams) (string, func(), int, error) {
		builds++
		tmpFile, err := os.CreateTemp(cfg.Printing.TempDir, "render-*.pdf")
		if err != nil {
			return "", nil, 0, err
		}
		tmpPath := tmpFile.Name()
		if _, err := tmpFile.WriteString("%PDF-1.7\nrendered\n"); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
			return "", nil, 0, err
		}
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return "", nil, 0, err
		}
		return tmpPath, func() { _ = os.Remove(tmpPath) }, 2, nil
	}

	params := previewRenderParams{Pages: "1", Nup: 1, Scale: 100, Copies: 2, Collate: true, Direction: "horizontal"}
	first, err := store.ensureRender(artifact, params)
	if err != nil {
		t.Fatalf("ensureRender first: %v", err)
	}
	second, err := store.ensureRender(artifact, params)
	if err != nil {
		t.Fatalf("ensureRender second: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("render IDs differ: %s vs %s", first.ID, second.ID)
	}
	if first.PageCount != 2 {
		t.Fatalf("PageCount = %d, want 2", first.PageCount)
	}
	if builds != 1 {
		t.Fatalf("render builds = %d, want 1", builds)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("render file missing: %v", err)
	}
}

func TestPreviewArtifactStoreEnsureRenderUsesDefaultBuilder(t *testing.T) {
	prevCfg := getConfig()
	cfg := testPreviewArtifactConfig(t)
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })

	store := newPreviewArtifactStore(cfg)
	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(sourcePath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}
	artifact, err := store.saveCanonical(sourcePath, "upload", "source.pdf", "", 1)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	render, err := store.ensureRender(artifact, previewRenderParams{
		Nup:       1,
		Scale:     100,
		Copies:    2,
		Collate:   true,
		Direction: "horizontal",
	})
	if err != nil {
		t.Fatalf("ensureRender: %v", err)
	}
	if render.PageCount != 2 {
		t.Fatalf("PageCount = %d, want 2", render.PageCount)
	}
	if got, err := countPDFPages(render.Path); err != nil {
		t.Fatalf("countPDFPages render: %v", err)
	} else if got != 2 {
		t.Fatalf("render pdf pages = %d, want 2", got)
	}
}

func testPreviewArtifactConfig(t *testing.T) *config.Config {
	t.Helper()
	tempDir := t.TempDir()
	return &config.Config{
		Printing: config.PrintingConfig{
			TempDir:            tempDir,
			PreviewArtifactTTL: "30m",
			MaxCopies:          10,
			MaxPDFPages:        500,
			MaxUploadBytes:     50 * 1024 * 1024,
		},
		Printers: []config.PrinterConfig{
			{ID: "printer-1", URI: "ipp://localhost:631/printers/printer-1", Visible: true},
		},
	}
}
