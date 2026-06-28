package api

import (
	"path/filepath"
	"strings"
	"testing"

	"goprint/config"
)

func TestNormalizePreviewRenderParamsDefaults(t *testing.T) {
	params, err := normalizePreviewRenderParams(&config.Config{
		Printing: config.PrintingConfig{MaxCopies: 10},
	}, previewRenderParamsInput{})
	if err != nil {
		t.Fatalf("normalizePreviewRenderParams: %v", err)
	}

	want := previewRenderParams{
		Pages:     "",
		Nup:       1,
		Scale:     100,
		Copies:    1,
		Collate:   true,
		Direction: "horizontal",
	}
	if params != want {
		t.Fatalf("params = %+v, want %+v", params, want)
	}
}

func TestNormalizePreviewRenderParamsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		in   previewRenderParamsInput
		want string
	}{
		{name: "nup", in: previewRenderParamsInput{Nup: 3}, want: "nup"},
		{name: "scale", in: previewRenderParamsInput{Scale: 9}, want: "scale"},
		{name: "copies", in: previewRenderParamsInput{Copies: 11}, want: "copies"},
		{name: "direction", in: previewRenderParamsInput{Direction: "diagonal"}, want: "direction"},
	}

	cfg := &config.Config{Printing: config.PrintingConfig{MaxCopies: 10}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizePreviewRenderParams(cfg, tt.in)
			if err == nil {
				t.Fatal("normalizePreviewRenderParams succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestBuildRenderedPDFAppliesCopies(t *testing.T) {
	prevCfg := getConfig()
	cfg := &config.Config{Printing: config.PrintingConfig{TempDir: t.TempDir(), MaxCopies: 10}}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(sourcePath, 2); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}

	renderedPath, cleanup, pageCount, err := buildRenderedPDF(sourcePath, previewRenderParams{
		Nup:       1,
		Scale:     100,
		Copies:    3,
		Collate:   true,
		Direction: "horizontal",
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("buildRenderedPDF: %v", err)
	}
	if renderedPath == sourcePath {
		t.Fatal("buildRenderedPDF returned source path for copies=3")
	}
	if pageCount != 6 {
		t.Fatalf("pageCount = %d, want 6", pageCount)
	}
	if got, err := countPDFPages(renderedPath); err != nil {
		t.Fatalf("countPDFPages rendered: %v", err)
	} else if got != 6 {
		t.Fatalf("rendered page count = %d, want 6", got)
	}
}
