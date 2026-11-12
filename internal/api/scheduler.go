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

// RunScheduler triggers a cycle every interval until ctx is cancelled. If a
// cycle is already running, the tick is skipped (design.md §11). With
// interval 0 it does nothing: manual sync only.
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

// ReadCommands reads commands from r, one per line, until EOF or until ctx
// is cancelled. It is the app's stdin channel: the hassio.addon_stdin action
// with {"input": "sync"} triggers a cycle. Commands it does not recognize are
// passed to onUnknown.
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
			// HA sends the input as JSON (a quoted string).
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
