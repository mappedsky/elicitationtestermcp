// Command elicitationtestermcp serves the MCP elicitation test catalogue.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mappedsky/elicitationtestermcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTransport   = "http"
	defaultHTTPAddress = "127.0.0.1:8080"
	shutdownTimeout    = 10 * time.Second
)

func main() {
	log.SetFlags(0)
	log.SetPrefix(mcpserver.Name + ": ")
	log.SetOutput(os.Stderr)

	defaults := mcpserver.DefaultConfig()
	httpDefaults := mcpserver.DefaultHTTPConfig()

	transport := flag.String("transport", envOrDefault("ELICIT_MCP_TRANSPORT", defaultTransport), "MCP transport: http or stdio")
	httpAddress := flag.String("http-address", envOrDefault("ELICIT_MCP_HTTP_ADDRESS", defaultHTTPAddress), "streamable HTTP listen address")
	httpPath := flag.String("http-path", envOrDefault("ELICIT_MCP_HTTP_PATH", httpDefaults.Path), "stateless endpoint path; the only one that serves protocol version 2026-07-28")
	legacyPath := flag.String("legacy-http-path", envOrDefault("ELICIT_MCP_LEGACY_HTTP_PATH", httpDefaults.LegacyPath), "stateful endpoint path, capped at protocol version 2025-11-25 and the only one that can send elicitation/create requests; empty disables it")
	authorizePath := flag.String("authorize-path", envOrDefault("ELICIT_MCP_AUTHORIZE_PATH", httpDefaults.AuthorizePath), "path of the stand-in authorization page the URL scenarios point at; empty disables it")
	allowedURL := flag.String("allowed-url", envOrDefault("ELICIT_MCP_ALLOWED_URL", defaults.AllowedURL), "the URL the client under test is configured to trust; every URL scenario is derived from it")
	maxFields := flag.Int("max-form-fields", intFromEnv("ELICIT_MCP_MAX_FORM_FIELDS", defaults.MaxFormFields), "field count for the form/max_fields scenario; one more is form/too_many_fields")
	stateTTL := flag.Duration("state-ttl", durationFromEnv("ELICIT_MCP_STATE_TTL", defaults.StateTTL), "how long a signed request state stays answerable; it must exceed however long the client parks an elicitation")
	stateSecret := flag.String("state-secret", os.Getenv("ELICIT_MCP_STATE_SECRET"), "fixed signing key for request states; empty means a random per-process key, which invalidates parked elicitations on restart")
	logSize := flag.Int("log-size", intFromEnv("ELICIT_MCP_LOG_SIZE", defaults.LogSize), "exchanges retained by exchange_log")
	showVersion := flag.Bool("version", false, "print the server version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(mcpserver.Version)
		return
	}

	// The allowed URL is the anchor for every URL scenario, so a URL that
	// cannot be the trusted one is a startup error rather than a surprise
	// halfway through a run.
	if err := validateAllowedURL(*allowedURL); err != nil {
		log.Fatal(err)
	}

	server, err := mcpserver.New(mcpserver.Config{
		AllowedURL:    *allowedURL,
		MaxFormFields: *maxFields,
		StateTTL:      *stateTTL,
		StateSecret:   *stateSecret,
		LogSize:       *logSize,
	})
	if err != nil {
		log.Fatal(err)
	}
	if *stateSecret == "" {
		log.Print("request states are signed with a random key; set -state-secret to keep parked elicitations answerable across a restart")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpConfig := mcpserver.HTTPConfig{Path: *httpPath, LegacyPath: *legacyPath, AuthorizePath: *authorizePath}
	if err := run(ctx, server, *transport, *httpAddress, httpConfig); err != nil {
		log.Fatal(err)
	}
}

func validateAllowedURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("allowed URL: %w", err)
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return fmt.Errorf("allowed URL must be http or https, got %q", parsed.Scheme)
	case parsed.Host == "":
		return fmt.Errorf("allowed URL must name a host")
	case parsed.User != nil:
		return fmt.Errorf("allowed URL must not embed credentials; it is the URL scenarios are expected to pass")
	case parsed.Fragment != "":
		return fmt.Errorf("allowed URL must not carry a fragment; it is the URL scenarios are expected to pass")
	}
	return nil
}

func run(ctx context.Context, server *mcp.Server, transport, address string, httpConfig mcpserver.HTTPConfig) error {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "stdio":
		err := server.Run(ctx, &mcp.StdioTransport{})
		if err != nil && ctx.Err() != nil {
			return nil
		}
		return err
	case "http":
		return runHTTP(ctx, server, address, httpConfig)
	default:
		return fmt.Errorf("unsupported transport %q; supported transports: http, stdio", transport)
	}
}

func runHTTP(ctx context.Context, mcpServer *mcp.Server, address string, httpConfig mcpserver.HTTPConfig) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("HTTP address must not be empty")
	}
	handler, err := mcpserver.NewHTTPHandler(mcpServer, httpConfig)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	defer listener.Close()

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownDone <- httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("streamable HTTP listening on http://%s%s (stateless, serves 2026-07-28)", listener.Addr(), httpConfig.Path)
	if httpConfig.LegacyPath != "" {
		log.Printf("legacy endpoint on http://%s%s (stateful, capped at 2025-11-25, sends elicitation/create)", listener.Addr(), httpConfig.LegacyPath)
	}
	if httpConfig.AuthorizePath != "" {
		log.Printf("authorization stand-in page on http://%s%s", listener.Addr(), httpConfig.AuthorizePath)
	}
	err = httpServer.Serve(listener)
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve streamable HTTP: %w", err)
	}
	if err := <-shutdownDone; err != nil {
		return fmt.Errorf("shut down streamable HTTP: %w", err)
	}
	return nil
}

func intFromEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("%s must be an integer, got %q", name, raw)
	}
	return value
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		log.Fatalf("%s must be a Go duration such as 2h, got %q", name, raw)
	}
	return value
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
