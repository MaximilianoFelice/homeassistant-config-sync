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

// cmdServe levanta la API, el scheduler y (opcional) el canal de stdin.
// Termina con SIGINT/SIGTERM esperando a que el ciclo en curso termine.
func cmdServe(g globalFlags, stdout, stderr io.Writer) int {
	e, cfg, ok := newEngine(g, stderr)
	if !ok {
		return exitError
	}
	tok, err := engine.APIToken(g.stateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs: no hay token de la API (¿corriste hcs init?):", err)
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
				logger.Printf("sync %s (%s): %s, estado %s: %s", j.ID, j.Trigger, j.Status, state, j.Error)
				return
			}
			logger.Printf("sync %s (%s): %s, estado %s", j.ID, j.Trigger, j.Status, state)
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
			logger.Printf("la API se cayó: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	interval := time.Duration(cfg.SyncIntervalMinutes) * time.Minute
	logger.Printf("API en http://%s (token en %s/api_token); sync programado: %s", ln.Addr(), g.stateDir, orManual(interval))
	go api.RunScheduler(ctx, srv, interval, func() { logger.Print("tick salteado: hay un sync corriendo") })
	if g.stdin {
		go api.ReadCommands(ctx, os.Stdin, srv,
			func(cmd string) { logger.Printf("comando de stdin desconocido: %q", cmd) },
			func() { logger.Print("sync por stdin ignorado: ya hay uno corriendo") })
	}
	if g.syncOnStart {
		if _, err := srv.Trigger("start", engine.SyncOptions{}); err != nil {
			logger.Printf("no se pudo lanzar el sync inicial: %v", err)
		}
	}

	<-ctx.Done()
	logger.Print("apagando: espero a que termine el sync en curso")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	srv.Wait()
	return exitOK
}

func orManual(d time.Duration) string {
	if d <= 0 {
		return "desactivado (solo manual)"
	}
	return "cada " + d.String()
}
