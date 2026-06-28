package api

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goprint/config"
)

func InitOfficeCacheCleaner(cfg *config.Config) {
	maxAge, err := officeCacheMaxAge(cfg)
	if err != nil {
		log.Printf("[office-cache] invalid cache max age: %v", err)
		return
	}
	if maxAge <= 0 {
		return
	}

	if removed, err := cleanupOfficeConversionCache(cfg, time.Now()); err != nil {
		log.Printf("[office-cache] startup cleanup failed: %v", err)
	} else if removed > 0 {
		log.Printf("[office-cache] startup cleanup removed files=%d", removed)
	}

	interval := officeCacheCleanupInterval(maxAge)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if removed, err := cleanupOfficeConversionCache(cfg, time.Now()); err != nil {
				log.Printf("[office-cache] cleanup failed: %v", err)
			} else if removed > 0 {
				log.Printf("[office-cache] cleanup removed files=%d", removed)
			}
		}
	}()
}

func cleanupOfficeConversionCache(cfg *config.Config, now time.Time) (int, error) {
	maxAge, err := officeCacheMaxAge(cfg)
	if err != nil || maxAge <= 0 {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now()
	}

	outputDir := ""
	if cfg != nil {
		outputDir = strings.TrimSpace(cfg.OfficeConversion.OutputDir)
	}
	if outputDir == "" {
		return 0, nil
	}

	cacheDir := filepath.Join(outputDir, "cache")
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}

	cutoff := now.Add(-maxAge)
	removed := 0
	err = filepath.WalkDir(cacheDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

func officeCacheMaxAge(cfg *config.Config) (time.Duration, error) {
	if cfg == nil {
		return 0, nil
	}
	raw := strings.TrimSpace(cfg.OfficeConversion.CacheMaxAge)
	if raw == "" {
		return 0, nil
	}
	return time.ParseDuration(raw)
}

func officeCacheCleanupInterval(maxAge time.Duration) time.Duration {
	interval := maxAge / 4
	if interval < time.Hour {
		return time.Hour
	}
	if interval > 24*time.Hour {
		return 24 * time.Hour
	}
	return interval
}
