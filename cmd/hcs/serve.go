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
)

// cmdServe starts the API, the scheduler and (optionally) the stdin channel.
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

	srv := api.New(api.Options{
		Config: cfg, StateDir: g.stateDir, Sync: e.Sync, Token: tok,
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

	ln, err := net.Listen("tcp", g.listen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return exitError
	}
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
	if g.stdin {
		go api.ReadCommands(ctx, os.Stdin, srv,
			func(cmd string) { logger.Printf("unknown stdin command: %q", cmd) },
			func() { logger.Print("stdin sync ignored: one is already running") })
	}
	if g.syncOnStart {
		if _, err := srv.Trigger("start", engine.SyncOptions{}); err != nil {
			logger.Printf("could not start the initial sync: %v", err)
		}
	}

	<-ctx.Done()
	logger.Print("shutting down: waiting for the running sync to finish")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	srv.Wait()
	return exitOK
}

func orManual(d time.Duration) string {
	if d <= 0 {
		return "disabled (manual only)"
	}
	return "every " + d.String()
}
