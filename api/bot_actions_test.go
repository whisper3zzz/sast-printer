package api

import (
	"testing"
	"time"

	"goprint/config"
)

func TestBotSelectedDuplexModeHonorsSingleSidedSelection(t *testing.T) {
	if got := botSelectedDuplexMode("off", 8); got != "off" {
		t.Fatalf("duplex mode = %s, want off", got)
	}
}

func TestBotSelectedDuplexModeKeepsManualSelection(t *testing.T) {
	if got := botSelectedDuplexMode("manual", 8); got != "manual" {
		t.Fatalf("duplex mode = %s, want manual", got)
	}
}

func TestBotSelectedDuplexModeForcesSinglePageToSingleSided(t *testing.T) {
	if got := botSelectedDuplexMode("manual", 1); got != "off" {
		t.Fatalf("duplex mode = %s, want off", got)
	}
}

func TestHandleBotPrintReleasesSessionActionAfterProcessingFailure(t *testing.T) {
	sessionID := "test-session-processing-failure"
	deleteBotSession(sessionID)
	defer deleteBotSession(sessionID)

	cfg := &config.Config{
		Printing: config.PrintingConfig{
			MaxCopies: 100,
		},
		Printers: []config.PrinterConfig{
			{
				ID:         "printer-1",
				URI:        "ipp://localhost:631/printers/printer-1",
				Visible:    true,
				DuplexMode: "off",
			},
		},
	}
	saveBotSession(sessionID, botCardSession{
		SourcePath:      "/tmp/goprint-nonexistent-source.pdf",
		Filename:        "source.pdf",
		PrinterID:       "printer-1",
		ChatID:          "chat-1",
		ChatType:        "group",
		ReplyMessageID:  "msg-1",
		RequesterOpenID: "",
		TotalPages:      2,
		CreatedAt:       time.Now(),
	})

	handleBotPrint(cfg, map[string]interface{}{
		"session_id": sessionID,
		"printer_id": "printer-1",
		"copies":     "1",
		"pages":      "1",
		"nup":        "1",
		"scale":      "100",
		"duplex":     "off",
	}, "", false)

	session, ok := getBotSession(sessionID)
	if !ok {
		t.Fatal("bot session was unexpectedly removed")
	}
	if session.ActionInProgress {
		t.Fatal("bot session action remained in progress after processing failure")
	}
}
