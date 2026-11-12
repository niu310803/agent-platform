package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileReimportWaitsForCompletePackageAndPreservesGeneration(t *testing.T) {
	s, files := deletionFixture(t)
	if err := DeletePackage(t.Context(), s, "demo", nil, nil); err != nil {
		t.Fatal(err)
	}
	deleted, _ := ReadStateControl(s.StateRoot, "demo")
	manifest := filepath.Join(s.ExternalRoot, "demo", "connector.json")
	writeDeletionFixtureFile(t, manifest, files[manifest])
	if err := s.ReconcileInstallations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadStateControl(s.StateRoot, "demo"); got != deleted {
		t.Fatalf("incomplete package was activated: %+v", got)
	}
	cli := filepath.Join(s.ExternalRoot, "demo", "cli.json")
	writeDeletionFixtureFile(t, cli, files[cli])
	for range 2 {
		if err := s.ReconcileInstallations(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, err := ReadStateControl(s.StateRoot, "demo")
		if err != nil || got.Deleted || got.Deleting || got.Generation != deleted.Generation || got.Generation == "" {
			t.Fatalf("bad reactivation: %+v %v", got, err)
		}
	}
	for _, root := range []string{s.StateRoot, s.InstallRoot()} {
		if _, err := os.Stat(filepath.Join(root, "demo")); !os.IsNotExist(err) {
			t.Fatalf("reactivation restored private state/install: %s %v", root, err)
		}
	}
}

func TestFileReimportDoesNotBypassDeletionOrPublication(t *testing.T) {
	for _, scenario := range []string{"deleting", "cleanup", "publishing"} {
		t.Run(scenario, func(t *testing.T) {
			s, files := deletionFixture(t)
			if err := DeletePackage(t.Context(), s, "demo", nil, nil); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"connector.json", "cli.json"} {
				path := filepath.Join(s.ExternalRoot, "demo", name)
				writeDeletionFixtureFile(t, path, files[path])
			}
			control, _ := ReadStateControl(s.StateRoot, "demo")
			switch scenario {
			case "deleting":
				control.Deleting = true
				if err := WriteStateControl(s.StateRoot, "demo", control); err != nil {
					t.Fatal(err)
				}
			case "cleanup":
				if err := writeDeletionRecord(s, deletionRecord{ID: "demo", Generation: control.Generation, RootsHash: deletionRootsHash(s), Stages: []deletionStage{{Kind: "source", Name: deletionStagePrefix("demo") + "123"}}}); err != nil {
					t.Fatal(err)
				}
			case "publishing":
				release, err := AcquireOperation(s.ExternalRoot, "demo")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if err := s.ReconcileInstallations(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got, _ := ReadStateControl(s.StateRoot, "demo"); got != control {
				t.Fatalf("bypassed %s: %+v", scenario, got)
			}
		})
	}
}

func TestFileReimportHonorsCancellation(t *testing.T) {
	s, _ := deletionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ReconcileInstallations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
