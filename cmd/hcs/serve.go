package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/api"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
)

// cmdServe starts the API and the scheduler.
// On SIGINT/SIGTERM it exits after waiting for the running cycle to finish.
func cmdServe(g globalFlags, stdout, stderr io.Writer) int {
	e, cfg, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	tok, err := engine.APIToken(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs: no API token (did you run hcs init?):", err)
		return exitError
	}
	logger := log.New(stdout, "", log.LstdFlags)

	ln, err := net.Listen("tcp", g.listen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
	host, _ := os.Hostname()
	srv := api.New(api.Options{
		Config: cfg, StateDir: g.stateDir, Sync: e.Sync, Token: tok,
		Host: host, Port: listenPort(ln),
		OnJob: func(j *api.Job) {
			state := "-"
			if j.Report != nil {
				state = j.Report.State
			}
			if j.Error != "" {
				logger.Printf("sync %s (%s): %s, state %s: %s", j.ID, j.Trigger, j.Status, state, j.Error)
				return
			}
			logger.Printf("sync %s (%s): %s, state %s", j.ID, j.Trigger, j.Status, state)
		},
	})

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("the API went down: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	interval := time.Duration(cfg.SyncIntervalMinutes) * time.Minute
	logger.Printf("API at http://%s (token in %s/api_token); scheduled sync: %s", ln.Addr(), g.stateDir, orManual(interval))
	go api.RunScheduler(ctx, srv, interval, func() { logger.Print("tick skipped: a sync is already running") })
	if g.syncOnStart {
		if _, err := srv.Trigger("start", engine.SyncOptions{}); err != nil {
			logger.Printf("could not start the initial sync: %v", err)
		}
	}
	if g.discovery {
		go announce(ctx, logger, ln, tok)
	}

	<-ctx.Done()
	logger.Print("shutting down: waiting for the running sync to finish")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	srv.Wait()
	return exitOK
}

// DiscoveryService is the Supervisor discovery service, the domain of the
// Home Assistant integration (custom_components/config_sync).
const DiscoveryService = "config_sync"

// announce publishes this app through Supervisor discovery so the
// config_sync integration can set itself up: the app's hostname on the
// internal network, the API port and the API token. Retries a few times,
// since the Supervisor may still be starting.
func announce(ctx context.Context, logger *log.Logger, ln net.Listener, token string) {
	sup, err := ha.NewSupervisorFromEnv("")
	if err != nil {
		logger.Printf("discovery skipped: %v", err)
		return
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Printf("discovery skipped: %v", err)
		return
	}
	port := listenPort(ln)
	cfg := map[string]any{"host": host, "port": port, "token": token}
	for attempt := 1; attempt <= 5; attempt++ {
		if err = sup.PublishDiscovery(ctx, DiscoveryService, cfg); err == nil {
			logger.Printf("announced to Home Assistant (discovery %q, host %s:%d)", DiscoveryService, host, port)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt) * 10 * time.Second):
		}
	}
	logger.Printf("discovery failed (the config_sync integration can still be added by hand): %v", err)
}

func listenPort(ln net.Listener) int {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 8099
}

func orManual(d time.Duration) string {
	if d <= 0 {
		return "disabled (manual only)"
	}
	return "every " + d.String()
}
