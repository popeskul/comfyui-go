package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"local/comfyui-lab/internal/jobs"
)

type config struct {
	addr, token, db, upstream, workflow string
	queue, maxJobs                      int
	maxBytes                            int64
	maxAge                              time.Duration
}

func loadConfig(get func(string) string) (config, error) {
	value := func(key, fallback string) string {
		v := get(key)
		if v == "" {
			return fallback
		}

		return v
	}

	c := config{
		addr:     value("ADDR", "127.0.0.1:8088"),
		token:    get("API_TOKEN"),
		db:       value("DB_PATH", ".data/jobs.db"),
		upstream: value("COMFY_URL", "http://127.0.0.1:8188"),
		workflow: value("WORKFLOW_PATH", "workflow.json"),
	}

	if len(c.token) < 32 || len(c.token) > 256 || strings.ContainsAny(c.token, "\r\n") {
		return c, fmt.Errorf("API_TOKEN must contain 32 to 256 characters")
	}

	var err error

	c.queue, err = strconv.Atoi(value("MAX_QUEUE", "32"))
	if err != nil || c.queue < 1 || c.queue > 10000 {
		return c, fmt.Errorf("MAX_QUEUE must be 1..10000")
	}

	c.maxJobs, err = strconv.Atoi(value("MAX_JOBS", "1000"))
	if err != nil || c.maxJobs < 1 || c.maxJobs > 100000 {
		return c, fmt.Errorf("MAX_JOBS must be 1..100000")
	}

	c.maxBytes, err = strconv.ParseInt(value("MAX_RESULT_BYTES", "536870912"), 10, 64)
	if err != nil || c.maxBytes < 1 || c.maxBytes > 1<<40 {
		return c, fmt.Errorf("MAX_RESULT_BYTES must be 1..1099511627776")
	}

	c.maxAge, err = time.ParseDuration(value("JOB_TIMEOUT", "30m"))
	if err != nil || c.maxAge < time.Second || c.maxAge > 24*time.Hour {
		return c, fmt.Errorf("JOB_TIMEOUT must be 1s..24h")
	}

	if _, _, err = net.SplitHostPort(c.addr); err != nil {
		return c, fmt.Errorf("invalid ADDR: %w", err)
	}

	return c, nil
}

func run(ctx context.Context, c config) error {
	template, err := os.ReadFile(c.workflow)
	if err != nil {
		return err
	}

	store, err := jobs.Open(c.db, c.queue, c.maxBytes, c.maxJobs)
	if err != nil {
		return err
	}

	defer store.Close()

	backend, err := jobs.NewComfy(c.upstream, 10*time.Second)
	if err != nil {
		return err
	}

	if err = store.Prune(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		return err
	}

	if err = store.Recover(ctx); err != nil {
		return err
	}

	worker := jobs.Worker{Store: store, Backend: backend, MaxAge: c.maxAge}

	api, err := jobs.NewAPI(store, worker, template, c.token)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", c.addr)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16384,
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		maintenance := time.NewTicker(time.Hour)
		defer maintenance.Stop()

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			if err := worker.Step(workerCtx); err != nil && workerCtx.Err() == nil {
				slog.Error("worker step failed", "error", err)
			}

			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			case <-maintenance.C:
				if err := store.Prune(workerCtx, time.Now().Add(-30*24*time.Hour)); err != nil && workerCtx.Err() == nil {
					slog.Error("retention failed", "error", err)
				}
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	slog.Info("image service listening", "address", listener.Addr().String())

	select {
	case <-ctx.Done():
		err = nil
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}

	api.Drain()
	stopWorker() // In-flight upstream IDs remain durable; a restart resumes reads.

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()

		if err == nil {
			err = shutdownErr
		}
	}

	wg.Wait()
	slog.Info("image service stopped")

	return err
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	c, err := loadConfig(os.Getenv)
	if err != nil {
		slog.Error("configuration invalid", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = run(ctx, c); err != nil {
		slog.Error("service failed", "error", err)
		os.Exit(1)
	}
}
