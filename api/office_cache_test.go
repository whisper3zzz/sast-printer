package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"goprint/config"
)

func TestCleanupOfficeConversionCacheRemovesExpiredFilesOnly(t *testing.T) {
	outputDir := t.TempDir()
	oldPath := filepath.Join(outputDir, "cache", "source", "old.docx")
	newPath := filepath.Join(outputDir, "cache", "pdf", "new.pdf")

	for _, path := range []string{oldPath, newPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("cache"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.Local)
	if err := os.Chtimes(oldPath, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(newPath, now.Add(-30*time.Minute), now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("chtimes new: %v", err)
	}

	removed, err := cleanupOfficeConversionCache(&config.Config{
		OfficeConversion: config.OfficeConversionConfig{
			OutputDir:   outputDir,
			CacheMaxAge: "1h",
		},
	}, now)
	if err != nil {
		t.Fatalf("cleanupOfficeConversionCache: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old cache file still exists or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new cache file missing: %v", err)
	}
}
