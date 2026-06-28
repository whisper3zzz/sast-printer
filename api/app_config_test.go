package api

import (
	"os"
	"path/filepath"
	"testing"

	"goprint/config"
)

func TestParsePrinterURIIPPSUsesTLS(t *testing.T) {
	host, port, printerName, useTLS, err := parsePrinterURI("ipps://cups.example.test:8631/printers/secure")
	if err != nil {
		t.Fatalf("parsePrinterURI: %v", err)
	}
	if host != "cups.example.test" || port != 8631 || printerName != "secure" || !useTLS {
		t.Fatalf("parsePrinterURI = host %q port %d printer %q tls %t", host, port, printerName, useTLS)
	}
}

func TestResolveVisiblePrinterRejectsHiddenPrinter(t *testing.T) {
	prevCfg := getConfig()
	SetConfig(&config.Config{
		Printers: []config.PrinterConfig{
			{ID: "hidden", URI: "ipp://localhost:631/printers/hidden", Visible: false},
			{ID: "visible", URI: "ipp://localhost:631/printers/visible", Visible: true},
		},
	})
	defer SetConfig(prevCfg)

	if _, err := resolveVisiblePrinter("visible"); err != nil {
		t.Fatalf("resolveVisiblePrinter visible: %v", err)
	}
	if _, err := resolveVisiblePrinter("hidden"); err == nil {
		t.Fatal("resolveVisiblePrinter hidden succeeded")
	}
}

func TestInitTempDirPreservesUnexpiredPreviewArtifacts(t *testing.T) {
	prevCfg := getConfig()
	cfg := &config.Config{
		Printing: config.PrintingConfig{
			TempDir:            t.TempDir(),
			PreviewArtifactTTL: "30m",
			MaxCopies:          10,
			MaxPDFPages:        500,
		},
		Printers: []config.PrinterConfig{
			{ID: "printer-1", URI: "ipp://localhost:631/printers/printer-1", Visible: true},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(prevCfg) })

	ordinaryTemp := filepath.Join(cfg.Printing.TempDir, "ordinary.tmp")
	if err := os.MkdirAll(cfg.Printing.TempDir, 0o755); err != nil {
		t.Fatalf("mkdir temp dir: %v", err)
	}
	if err := os.WriteFile(ordinaryTemp, []byte("remove me"), 0o644); err != nil {
		t.Fatalf("write ordinary temp: %v", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "source.pdf")
	if err := createBlankPDF(sourcePath, 1); err != nil {
		t.Fatalf("createBlankPDF: %v", err)
	}
	artifact, err := newPreviewArtifactStore(cfg).saveCanonical(sourcePath, "upload", "source.pdf", "", 1)
	if err != nil {
		t.Fatalf("saveCanonical: %v", err)
	}

	if err := InitTempDir(); err != nil {
		t.Fatalf("InitTempDir: %v", err)
	}

	if _, err := os.Stat(ordinaryTemp); !os.IsNotExist(err) {
		t.Fatalf("ordinary temp still exists or stat failed differently: %v", err)
	}
	if _, err := os.Stat(artifact.CanonicalPath); err != nil {
		t.Fatalf("preview artifact canonical was not preserved: %v", err)
	}
}
