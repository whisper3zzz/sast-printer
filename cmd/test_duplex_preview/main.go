package main

import (
	"fmt"
	"os"

	"goprint/api"
	"goprint/config"
)

func findPrinter(cfg *config.Config, id string) (config.PrinterConfig, bool) {
	for _, p := range cfg.Printers {
		if p.ID == id {
			return p, true
		}
	}
	return config.PrinterConfig{}, false
}

func runCase(sourcePath string, printerCfg config.PrinterConfig, copies int, collate bool, label string) error {
	firstPath, secondPath, cleanup, err := api.BuildManualDuplexPreview(sourcePath, printerCfg, "long-edge", copies, collate)
	if err != nil {
		return err
	}
	defer cleanup()

	savedFirst, err := api.SavePDFForTest(firstPath, label+"-first")
	if err != nil {
		return err
	}
	savedSecond, err := api.SavePDFForTest(secondPath, label+"-second")
	if err != nil {
		return err
	}

	fmt.Printf("case=%s copies=%d collate=%v\n", label, copies, collate)
	fmt.Printf("first pass:  %s\n", savedFirst)
	fmt.Printf("second pass: %s\n", savedSecond)
	return nil
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func main() {
	cfg, err := config.LoadFromFile("config.yaml")
	if err != nil {
		fmt.Printf("failed to load config: %v\n", err)
		os.Exit(1)
	}

	printerCfg, ok := findPrinter(cfg, "sast-color-printer")
	if !ok {
		fmt.Println("printer not found: sast-color-printer")
		os.Exit(1)
	}

	sourcePath := "printer_test_3.pdf"
	copies := 2
	collate := true

	if err := runCase(sourcePath, printerCfg, copies, collate, "manual-baseline-current-config"); err != nil {
		fmt.Printf("failed baseline case: %v\n", err)
		os.Exit(1)
	}

	firstPassOddCfg := printerCfg
	firstPassOddCfg.FirstPass = "odd"
	if err := runCase(sourcePath, firstPassOddCfg, copies, collate, "manual-first-pass-odd"); err != nil {
		fmt.Printf("failed first_pass case: %v\n", err)
		os.Exit(1)
	}

	padOffCfg := printerCfg
	padOffCfg.PadToEven = boolPtr(false)
	if err := runCase(sourcePath, padOffCfg, copies, collate, "manual-pad-to-even-false"); err != nil {
		fmt.Printf("failed pad_to_even case: %v\n", err)
		os.Exit(1)
	}

	rotateSecondCfg := printerCfg
	rotateSecondCfg.RotateSecondPass = true
	if err := runCase(sourcePath, rotateSecondCfg, copies, collate, "manual-rotate-second-pass-true"); err != nil {
		fmt.Printf("failed rotate_second_pass case: %v\n", err)
		os.Exit(1)
	}
}
