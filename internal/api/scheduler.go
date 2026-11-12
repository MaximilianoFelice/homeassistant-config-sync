package api

import (
	"context"
	"errors"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
)

// RunScheduler triggers a cycle every interval until ctx is cancelled. If a
// cycle is already running, the tick is skipped. With
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
