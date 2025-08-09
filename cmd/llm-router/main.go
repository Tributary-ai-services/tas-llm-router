package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/config"
	"github.com/tributary-ai/llm-router-waf/internal/providers/anthropic"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/server"
)

// Application represents the main application
type Application struct {
	config *config.Config
	router *routing.Router
	server *server.Server
	logger *logrus.Logger
}

// NewApplication creates a new application instance
func NewApplication(configPath string) (*Application, error) {
	// Load configuration
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	// Setup logger
	logger := logrus.New()
	if err := setupLogger(logger, cfg.Logging); err != nil {
		return nil, fmt.Errorf("failed to setup logger: %w", err)
	}

	// Create router
	routerInstance := routing.NewRouter(logger)

	// Register providers
	if err := registerProviders(routerInstance, cfg, logger); err != nil {
		return nil, fmt.Errorf("failed to register providers: %w", err)
	}

	// Create server
	serverInstance, err := server.NewServer(routerInstance, cfg.ToServerConfig(), logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	return &Application{
		config: cfg,
		router: routerInstance,
		server: serverInstance,
		logger: logger,
	}, nil
}

// Run starts the application
func (app *Application) Run() error {
	app.logger.Info("Starting LLM Router WAF")

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start server in a goroutine
	serverErrors := make(chan error, 1)
	go func() {
		app.logger.WithField("address", ":"+app.config.Server.Port).Info("HTTP server starting")
		if err := app.server.Start(); err != nil {
			serverErrors <- fmt.Errorf("server failed to start: %w", err)
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case err := <-serverErrors:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigChan:
		app.logger.WithField("signal", sig.String()).Info("Shutdown signal received")
	}

	// Graceful shutdown
	app.logger.Info("Starting graceful shutdown...")
	
	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 30*time.Second)
	defer shutdownCancel()

	// Shutdown server
	if err := app.server.Stop(shutdownCtx); err != nil {
		app.logger.WithError(err).Error("Server shutdown error")
		return fmt.Errorf("server shutdown failed: %w", err)
	}

	app.logger.Info("Graceful shutdown completed")
	return nil
}

// setupLogger configures the logger based on configuration
func setupLogger(logger *logrus.Logger, config config.LoggingConfig) error {
	// Set log level
	level, err := logrus.ParseLevel(config.Level)
	if err != nil {
		return fmt.Errorf("invalid log level %s: %w", config.Level, err)
	}
	logger.SetLevel(level)

	// Set log format
	switch config.Format {
	case "json":
		logger.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})
	case "text":
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: time.RFC3339,
		})
	default:
		return fmt.Errorf("invalid log format: %s", config.Format)
	}

	// Set output
	switch config.Output {
	case "stdout":
		logger.SetOutput(os.Stdout)
	case "stderr":
		logger.SetOutput(os.Stderr)
	default:
		// Assume it's a file path
		file, err := os.OpenFile(config.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return fmt.Errorf("failed to open log file %s: %w", config.Output, err)
		}
		logger.SetOutput(file)
	}

	return nil
}

// registerProviders registers all configured providers with the router
func registerProviders(router *routing.Router, cfg *config.Config, logger *logrus.Logger) error {
	providersRegistered := 0

	// Register OpenAI provider if configured
	if cfg.Providers.OpenAI != nil && cfg.Providers.OpenAI.APIKey != "" {
		openaiProvider := openai.NewOpenAIProvider(cfg.Providers.OpenAI, logger)
		router.RegisterProvider("openai", openaiProvider)
		logger.WithFields(logrus.Fields{
			"provider": "openai",
			"models":   len(cfg.Providers.OpenAI.Models),
		}).Info("OpenAI provider registered")
		providersRegistered++
	}

	// Register Anthropic provider if configured
	if cfg.Providers.Anthropic != nil && cfg.Providers.Anthropic.APIKey != "" {
		anthropicProvider := anthropic.NewAnthropicProvider(cfg.Providers.Anthropic, logger)
		router.RegisterProvider("anthropic", anthropicProvider)
		logger.WithFields(logrus.Fields{
			"provider": "anthropic",
			"models":   len(cfg.Providers.Anthropic.Models),
		}).Info("Anthropic provider registered")
		providersRegistered++
	}

	if providersRegistered == 0 {
		return fmt.Errorf("no providers were registered - check your configuration and API keys")
	}

	logger.WithField("count", providersRegistered).Info("Provider registration completed")
	return nil
}

// printUsage prints application usage information
func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\nOptions:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nEnvironment Variables:\n")
	fmt.Fprintf(os.Stderr, "  OPENAI_API_KEY         OpenAI API key\n")
	fmt.Fprintf(os.Stderr, "  ANTHROPIC_API_KEY      Anthropic API key\n")
	fmt.Fprintf(os.Stderr, "  LLM_ROUTER_PORT        Server port (default: 8080)\n")
	fmt.Fprintf(os.Stderr, "  LLM_ROUTER_LOG_LEVEL   Log level (debug,info,warn,error,fatal)\n")
	fmt.Fprintf(os.Stderr, "  LLM_ROUTER_LOG_FORMAT  Log format (json,text)\n")
	fmt.Fprintf(os.Stderr, "  LLM_ROUTER_DEFAULT_STRATEGY  Default routing strategy\n")
	fmt.Fprintf(os.Stderr, "\nExamples:\n")
	fmt.Fprintf(os.Stderr, "  %s --config configs/config.yaml\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  OPENAI_API_KEY=sk-xxx ANTHROPIC_API_KEY=sk-ant-xxx %s\n", os.Args[0])
}

func main() {
	// Parse command line flags
	var (
		configPath = flag.String("config", "", "Path to configuration file")
		showHelp   = flag.Bool("help", false, "Show help message")
		version    = flag.Bool("version", false, "Show version information")
	)
	flag.Parse()

	// Show help if requested
	if *showHelp {
		printUsage()
		os.Exit(0)
	}

	// Show version if requested
	if *version {
		fmt.Printf("LLM Router WAF v1.0.0\n")
		fmt.Printf("Build Date: %s\n", time.Now().Format("2006-01-02"))
		os.Exit(0)
	}

	// Create and run application
	app, err := NewApplication(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create application: %v\n", err)
		os.Exit(1)
	}

	// Run application
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Application error: %v\n", err)
		os.Exit(1)
	}
}