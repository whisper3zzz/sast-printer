package main

import (
	"context"
	"fmt"
	"goprint/api"
	"goprint/config"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func maybeStartOfficeConverter(ctx context.Context, cfg *config.Config) (*exec.Cmd, error) {
	if !cfg.OfficeConversion.Enabled || !cfg.OfficeConversion.StartWithServer {
		return nil, nil
	}

	formats := strings.Join(cfg.OfficeConversion.AcceptedFormats, ",")
	args := []string{
		"--listen", cfg.OfficeConversion.GRPCAddress,
		"--output-dir", cfg.OfficeConversion.OutputDir,
		"--max-workers", "1",
		"--accepted-formats", formats,
	}

	cmd := exec.CommandContext(ctx, cfg.OfficeConversion.ServiceScript, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	log.Printf("Office converter service started: %s %s", cfg.OfficeConversion.ServiceScript, strings.Join(args, " "))
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("Office converter service exited with error: %v", err)
			return
		}
		log.Printf("Office converter service exited")
	}()

	return cmd, nil
}

func main() {
	fmt.Println("GoPrint is starting...")

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.LoadFromFile(configPath)
	if err != nil {
		log.Fatalf("Failed to load config %s: %v", configPath, err)
	}
	api.SetConfig(cfg)

	if err := api.InitTempDir(); err != nil {
		log.Printf("Warning: failed to initialize temp directory: %v", err)
	}
	api.InitOfficeCacheCleaner(cfg)

	if _, err := maybeStartOfficeConverter(rootCtx, cfg); err != nil {
		log.Fatalf("Failed to start office converter service: %v", err)
	}

	api.InitJobStatusPoller(cfg)

	router := api.SetupRouter()
	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	fmt.Printf("Running on http://%s\n", addr)

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown failed: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start server: %v", err)
	}
}
