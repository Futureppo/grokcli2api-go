package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/config"
	appserver "github.com/Futureppo/grokcli2api-go/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	host := flag.String("host", "", "bind host (overrides GROK2API_HOST)")
	port := flag.Int("port", 0, "bind port (overrides GROK2API_PORT)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(config.Version)
		return
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port != 0 {
		if *port < 1 || *port > 65535 {
			fatal(fmt.Errorf("port must be between 1 and 65535"))
		}
		cfg.Port = *port
	}
	configureLogging(cfg.LogLevel)
	app, err := appserver.New(cfg)
	if err != nil {
		fatal(err)
	}
	defer app.Close()

	httpServer := &http.Server{
		Addr: cfg.Host + ":" + strconv.Itoa(cfg.Port), Handler: app.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
	}
	go func() {
		slog.Info("grok2api started", "version", config.Version, "address", "http://"+httpServer.Addr, "upstream", cfg.ChatProxyBaseURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdown); err != nil {
		slog.Error("shutdown", "error", err)
	}
}

func configureLogging(level string) {
	var value slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		value = slog.LevelDebug
	case "WARN", "WARNING":
		value = slog.LevelWarn
	case "ERROR":
		value = slog.LevelError
	default:
		value = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: value})))
}
func fatal(err error) { fmt.Fprintln(os.Stderr, "grok2api:", err); os.Exit(1) }
