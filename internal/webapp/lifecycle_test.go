package webapp

import (
	"context"
	"testing"
	"time"
)

func TestSingleStopDrainsLeasesAndRejectsOtherApplication(t *testing.T) {
	o := singleFixture(t)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RestoreDesired(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := s.Current()
	scope, err := s.Acquire(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Release()
	other := "webapp-fedcba9876543210"
	if _, err = s.Acquire(other); err == nil {
		t.Fatal("acquired another application")
	}
	if _, err = s.Start(context.Background(), other, StateRequest{}); err == nil {
		t.Fatal("started another application")
	}
	if err = s.Check(other, scope.Generation, scope.Policy.Revision); err == nil {
		t.Fatal("checked another application")
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Stop(context.Background(), e.ID, StateRequest{ExpectedRevision: e.Revision, ExpectedStateRevision: e.StateRevision})
		done <- err
	}()
	select {
	case <-scope.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not revoke generation")
	}
	select {
	case <-done:
		t.Fatal("stop returned before lease release")
	default:
	}
	scope.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if scope.Check() == nil {
		t.Fatal("revoked scope remained usable")
	}
}
func TestSingleReopenCreatesFreshGeneration(t *testing.T) {
	o := singleFixture(t)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RestoreDesired(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := s.Current()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RestoreDesired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Current().Generation == e.Generation {
		t.Fatal("reused generation")
	}
}
