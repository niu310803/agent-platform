package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	platform "agent-platform"
	"agent-platform/internal/app"
	"agent-platform/internal/builtins"
	"agent-platform/internal/config"
	"agent-platform/internal/httpclient"
	"agent-platform/internal/webapphost"
)

const gracefulShutdownTimeout = 3 * time.Second

type shutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(platform.Version())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "webapp" {
		if err := runWebAppCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "webapp-child" {
		if err := webapphost.RunChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			var code interface{ ExitCode() int }
			if errors.As(err, &code) {
				os.Exit(code.ExitCode())
			}
			os.Exit(78)
		}
		return
	}

	if len(os.Args) >= 2 && os.Args[1] == "runtime-recovery" {
		if err := runRuntimeRecovery(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "config-migrate" {
		if err := config.RunConfigMigration(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "builtins-manifest" {
		if err := builtins.RunManifestCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "connector-manage" {
		if err := runConnectorManagement(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "runtime-resource-sync" {
		// Resource validation resolves the same bundled connectors as startup.
		if _, err := builtins.ConfigureProcessPath(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := runRuntimeResourceSync(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	startedAt := time.Now()
	log.Printf("starting runtime: pid=%d", os.Getpid())
	if binDir, err := builtins.ConfigureProcessPath(); err != nil {
		log.Printf("configure builtin PATH: %v", err)
	} else if binDir != "" {
		log.Printf("builtin PATH enabled: %s", binDir)
	}

	configOptions, err := parseConfigOptions(os.Args[1:])
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	startupFailed := true
	defer func() {
		if startupFailed {
			os.Exit(1)
		}
	}()
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	appInitStartedAt := time.Now()
	application, err := app.New(rootCtx, configOptions)
	if err != nil {
		log.Fatalf("startup failed during app init after %s: %v", startupElapsed(appInitStartedAt), err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			log.Printf("app close: %v", err)
		}
	}()

	if configOptions.ReadyFile != "" {
		if err := os.Remove(configOptions.ReadyFile); err != nil && !os.IsNotExist(err) {
			log.Printf("remove stale ready file: %v", err)
			return
		}
	}

	server := &http.Server{
		Addr:              application.Config.ServerAddress(),
		Handler:           application.Router,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Printf("listen failed: %v", err)
		return
	}
	defer listener.Close()
	if application.Config.SingleWebApp() {
		ctx, cancel := context.WithTimeout(rootCtx, 45*time.Second)
		err = application.WaitSingleReady(ctx)
		cancel()
		if err != nil {
			log.Printf("WebApp startup failed: %v", err)
			return
		}
	}
	apiURL := listenerURL(listener.Addr())
	appURL := ""
	if application.Config.SingleWebApp() {
		appURL, err = application.Router.PrepareSingleOrigin(apiURL)
		if err != nil {
			log.Printf("WebApp origin: %v", err)
			return
		}
	}
	if configOptions.ReadyFile != "" {
		if err := writeReady(configOptions.ReadyFile, apiURL, appURL); err != nil {
			log.Printf("ready file: %v", err)
			return
		}
		defer os.Remove(configOptions.ReadyFile)
	}
	runtimeDescription := resolvedRuntimeDescription(application.Config)
	log.Printf(
		"server ready: %s runtimeMode=%s actionTransport=reverse-websocket addr=%s app_init=%s total=%s",
		runtimeDescription,
		application.Config.RuntimeMode,
		server.Addr,
		startupElapsed(appInitStartedAt),
		startupElapsed(startedAt),
	)

	go func() {
		listenStartedAt := time.Now()
		log.Printf("listening on %s (%s)", server.Addr, runtimeDescription)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen on %s failed after %s: %v", server.Addr, startupElapsed(listenStartedAt), err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if configOptions.ParentPID > 0 {
		go watchParent(rootCtx, configOptions.ParentPID, func() {
			select {
			case stop <- syscall.SIGTERM:
			case <-rootCtx.Done():
			}
		})
	}

	startupFailed = false
	shutdownStartedAt := time.Now()
	if err := waitForShutdown(server, cancelRoot, stop, gracefulShutdownTimeout, os.Exit); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Printf("shutdown complete in %s", startupElapsed(shutdownStartedAt))
}

func runHealthcheck() error {
	port := strings.TrimSpace(os.Getenv("SERVER_PORT"))
	if port == "" {
		port = "8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := httpclient.DirectClient(0).Do(request)
	if err != nil {
		return fmt.Errorf("runtime health probe failed: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime health probe returned %s", response.Status)
	}
	return nil
}

func parseConfigOptions(args []string) (config.LoadOptions, error) {
	fs := flag.NewFlagSet("agent-platform", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := config.LoadOptions{}
	fs.StringVar(&options.ConfigDir, "config-dir", "", "configuration root containing .env and configs/")
	fs.StringVar(&options.Port, "port", "", "server listen port")
	fs.StringVar(&options.SsoIdTokenFile, "sso-id-token-file", "", "absolute identity token file overriding the state directory default")
	fs.StringVar(&options.RuntimeMode, "runtime-mode", string(config.RuntimeModeStandalone), "runtime host mode: standalone or desktop")
	fs.StringVar(&options.WebAppHost, "webapp-host", string(config.WebAppHostDisabled), "WebApp host: disabled or single")
	fs.StringVar(&options.Listen, "listen", "", "server listen address (host:port, port 0 allowed)")
	fs.StringVar(&options.ReadyFile, "ready-file", "", "absolute path for atomic readiness JSON")
	fs.StringVar(&options.WebAppNode, "webapp-node", "", "absolute Node 24 launcher path")
	fs.IntVar(&options.ParentPID, "parent-pid", 0, "exit when parent process exits")
	if err := fs.Parse(args); err != nil {
		return config.LoadOptions{}, err
	}
	if remaining := fs.Args(); len(remaining) > 0 {
		return config.LoadOptions{}, fmt.Errorf("unexpected argument(s): %s", strings.Join(remaining, " "))
	}
	if options.Listen != "" {
		if _, _, err := net.SplitHostPort(options.Listen); err != nil {
			return config.LoadOptions{}, err
		}
		if options.Port != "" {
			return config.LoadOptions{}, fmt.Errorf("--port and --listen are mutually exclusive")
		}
	}
	if options.ReadyFile != "" && !filepath.IsAbs(options.ReadyFile) {
		return config.LoadOptions{}, fmt.Errorf("--ready-file must be absolute")
	}
	if options.ParentPID < 0 || options.ParentPID == 1 || options.ParentPID == os.Getpid() {
		return config.LoadOptions{}, fmt.Errorf("invalid --parent-pid")
	}
	return options, nil
}

func startupElapsed(startedAt time.Time) time.Duration {
	return time.Since(startedAt).Round(time.Millisecond)
}

func resolvedRuntimeDescription(cfg config.Config) string {
	if strings.HasPrefix(cfg.Paths.RegistriesDir, "/opt/") &&
		strings.HasPrefix(cfg.Paths.AgentsDir, "/opt/") &&
		strings.HasPrefix(cfg.Paths.ChatsDir, "/opt/") {
		return "mode=compose/container"
	}
	return "mode=local"
}

func waitForShutdown(server shutdownServer, cancelRoot context.CancelFunc, signals <-chan os.Signal, timeout time.Duration, exit func(int)) error {
	sig := <-signals
	log.Printf("shutdown signal received: %s", sig)
	if cancelRoot != nil {
		cancelRoot()
	}
	if exit != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case secondSig := <-signals:
				log.Printf("second shutdown signal received: %s, forcing exit", secondSig)
				// This is an intentional hard exit that skips deferred cleanup.
				exit(1)
			case <-done:
			}
		}()
	}
	return shutdownHTTPServer(server, timeout)
}

func shutdownHTTPServer(server shutdownServer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			return fmt.Errorf("graceful shutdown failed: %w (force close: %v)", err, closeErr)
		}
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	return nil
}
