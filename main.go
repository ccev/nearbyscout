package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	path := flag.String("config", "config.toml", "configuration file")
	flag.Parse()
	if err := run(*path); err != nil {
		slog.Error("nearbyscout stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	s, err := newService(cfg)
	if err != nil {
		return err
	}
	defer s.client.CloseIdleConnections()
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	var workers sync.WaitGroup
	for i := 0; i < cfg.Dragonite.Workers; i++ {
		workers.Go(func() { s.worker(workerCtx) })
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	slog.Info("nearbyscout listening", "address", listener.Addr().String())
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
running:
	for {
		select {
		case <-ctx.Done():
			break running
		case err = <-serveErr:
			break running
		case <-ticker.C:
			s.logActivity()
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		// Do not close the queue while timed-out handlers might still enqueue.
		_ = srv.Close()
		cancelWorkers()
		workers.Wait()
		return shutdownErr
	}
	close(s.queue)
	drained := make(chan struct{})
	go func() { workers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-shutdownCtx.Done():
		cancelWorkers()
		<-drained
		slog.Warn("shutdown deadline reached; pending scouts discarded", "queue_depth", len(s.queue))
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
