package api

import (
	"fmt"
	"os"
	"strings"

	"goprint/api/pdfutil"
	"goprint/config"
)

type previewRenderParamsInput struct {
	Pages     string `json:"pages"`
	Nup       int    `json:"nup"`
	Scale     int    `json:"scale"`
	Copies    int    `json:"copies"`
	Collate   *bool  `json:"collate"`
	Direction string `json:"direction"`
}

func normalizePreviewRenderParams(cfg *config.Config, raw previewRenderParamsInput) (previewRenderParams, error) {
	params := previewRenderParams{
		Pages:     strings.TrimSpace(raw.Pages),
		Nup:       raw.Nup,
		Scale:     raw.Scale,
		Copies:    raw.Copies,
		Collate:   true,
		Direction: strings.ToLower(strings.TrimSpace(raw.Direction)),
	}

	if params.Nup == 0 {
		params.Nup = 1
	}
	if params.Nup != 1 && !pdfutil.ValidNup(params.Nup) {
		return previewRenderParams{}, fmt.Errorf("nup must be 1, 2, 4, or 6")
	}

	if params.Scale == 0 {
		params.Scale = 100
	}
	if params.Scale < 10 || params.Scale > 400 {
		return previewRenderParams{}, fmt.Errorf("scale must be an integer between 10 and 400")
	}

	if params.Copies == 0 {
		params.Copies = 1
	}
	if params.Copies <= 0 {
		return previewRenderParams{}, fmt.Errorf("copies must be a positive integer")
	}
	maxCopies := 100
	if cfg != nil && cfg.Printing.MaxCopies > 0 {
		maxCopies = cfg.Printing.MaxCopies
	}
	if params.Copies > maxCopies {
		return previewRenderParams{}, fmt.Errorf("copies must be no more than %d", maxCopies)
	}

	if raw.Collate != nil {
		params.Collate = *raw.Collate
	}

	if params.Direction == "" {
		params.Direction = "horizontal"
	}
	if params.Direction != "horizontal" && params.Direction != "vertical" {
		return previewRenderParams{}, fmt.Errorf("direction must be horizontal or vertical")
	}

	return params, nil
}

func renderParamsWithoutCopies(params previewRenderParams) previewRenderParams {
	params.Copies = 1
	params.Collate = true
	return params
}

func buildRenderedPDF(sourcePath string, params previewRenderParams) (string, func(), int, error) {
	params = fillRenderParamDefaults(params)
	currentPath := sourcePath
	cleanups := make([]func(), 0, 4)

	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (string, func(), int, error) {
		cleanupAll()
		return "", nil, 0, err
	}

	if strings.TrimSpace(params.Pages) != "" {
		selectedPath, cleanupPages, err := extractPDFPages(currentPath, params.Pages)
		if err != nil {
			return fail(err)
		}
		if selectedPath != currentPath {
			cleanups = append(cleanups, cleanupPages)
			currentPath = selectedPath
		}
	}

	if params.Nup > 1 {
		nupPath, cleanupNup, err := applyNupLayout(currentPath, params.Nup, params.Direction)
		if err != nil {
			return fail(err)
		}
		if nupPath != currentPath {
			cleanups = append(cleanups, cleanupNup)
			currentPath = nupPath
		}
	}

	if params.Scale != 100 {
		scaledPath, cleanupScale, err := applyScalePercent(currentPath, params.Scale)
		if err != nil {
			return fail(err)
		}
		if scaledPath != currentPath {
			cleanups = append(cleanups, cleanupScale)
			currentPath = scaledPath
		}
	}

	if params.Copies > 1 {
		copiesPath, err := applyCopiesMode(currentPath, params.Copies, params.Collate)
		if err != nil {
			return fail(err)
		}
		if copiesPath != currentPath {
			cleanups = append(cleanups, func() { _ = os.Remove(copiesPath) })
			currentPath = copiesPath
		}
	}

	pageCount, err := countPDFPages(currentPath)
	if err != nil {
		return fail(err)
	}
	if pageCount <= 0 {
		return fail(fmt.Errorf("invalid rendered pdf page count: %d", pageCount))
	}

	return currentPath, cleanupAll, pageCount, nil
}

func fillRenderParamDefaults(params previewRenderParams) previewRenderParams {
	params.Pages = strings.TrimSpace(params.Pages)
	if params.Nup == 0 {
		params.Nup = 1
	}
	if params.Scale == 0 {
		params.Scale = 100
	}
	if params.Copies == 0 {
		params.Copies = 1
	}
	params.Direction = strings.ToLower(strings.TrimSpace(params.Direction))
	if params.Direction == "" {
		params.Direction = "horizontal"
	}
	return params
}
