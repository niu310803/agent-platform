package app

import (
	"context"
	"fmt"
	"time"
)

func (a *App) WaitSingleReady(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		{
			entry := a.webapps.Current()
			switch entry.ObservedState {
			case "running":
				return nil
			case "failed", "blocked":
				return fmt.Errorf("%s: %s", entry.ID, entry.LastError)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
