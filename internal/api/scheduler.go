package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
)

// RunScheduler dispara un ciclo cada interval hasta que se cancele ctx. Si
// hay un ciclo corriendo, el tick se saltea (design.md §11). Con interval 0
// no hace nada: sync solo manual.
func RunScheduler(ctx context.Context, s *Server, interval time.Duration, onSkip func()) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Trigger("schedule", engine.SyncOptions{}); errors.Is(err, ErrBusy) && onSkip != nil {
				onSkip()
			}
		}
	}
}

// ReadCommands lee comandos de r, uno por línea, hasta EOF o hasta que se
// cancele ctx. Es el canal de stdin de la app: la acción hassio.addon_stdin
// con {"input": "sync"} dispara un ciclo. Devuelve los comandos que no
// reconoce a onUnknown.
func ReadCommands(ctx context.Context, r io.Reader, s *Server, onUnknown func(string), onBusy func()) {
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			// HA manda el input como JSON (un string entre comillas).
			cmd := strings.Trim(strings.TrimSpace(line), `"`)
			switch cmd {
			case "":
			case "sync":
				if _, err := s.Trigger("stdin", engine.SyncOptions{}); errors.Is(err, ErrBusy) && onBusy != nil {
					onBusy()
				}
			default:
				if onUnknown != nil {
					onUnknown(cmd)
				}
			}
		}
	}
}
