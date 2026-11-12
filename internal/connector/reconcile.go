package connector

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
)

// ReconcileInstallations recognizes valid packages recreated by file writes.
// Reads of the catalog and credentials must never perform this mutation.
func (s Sources) ReconcileInstallations(ctx context.Context) error {
	if strings.TrimSpace(s.ExternalRoot) == "" {
		return nil
	}
	entries, err := os.ReadDir(s.ExternalRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := entry.Name()
		if !entry.IsDir() || !ValidID(id) || IsBuiltin(id) {
			continue
		}
		control, err := ReadStateControl(s.PersistentRoot(), id)
		if err == nil && !control.Deleted {
			continue
		}
		if err == nil {
			err = s.reconcileInstallation(ctx, id)
		}
		if err != nil {
			// Isolate incomplete packages and lifecycle recovery just as LoadAll
			// isolates invalid definitions. A managed publisher owns its own commit.
			if !errors.Is(err, ErrBusy) {
				log.Printf("[connectors] cannot reactivate %q: %v", id, err)
			}
		}
	}
	return ctx.Err()
}

func (s Sources) reconcileInstallation(ctx context.Context, id string) error {
	release, err := AcquireOperation(s.ExternalRoot, id)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := AcquireStateLock(ctx, s.PersistentRoot(), id)
	if err != nil {
		return err
	}
	defer unlock()
	// Revalidate under the same locks used by import and deletion. Incomplete
	// file writes must leave the deletion fence intact on both Windows and macOS.
	if _, err := s.Load(id); err != nil {
		return err
	}
	_, err = activatePackageState(s, id)
	return err
}

// Caller holds the operation and state locks. Keep the deletion generation so
// callbacks from the previous installation remain fenced after reactivation.
// changed also covers a write whose final directory sync failed (for rollback).
func activatePackageState(s Sources, id string) (changed bool, err error) {
	control, err := ReadStateControl(s.PersistentRoot(), id)
	if err != nil {
		return false, err
	}
	pending, err := readDeletionRecord(s, id)
	if err != nil {
		return false, err
	}
	if pending != nil {
		return false, ErrDeleteCleanup
	}
	if control.Deleting {
		return false, ErrBusy
	}
	if !control.Deleted {
		return false, nil
	}
	control.Deleted = false
	return true, WriteStateControl(s.PersistentRoot(), id, control)
}
