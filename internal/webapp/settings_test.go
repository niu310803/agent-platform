package webapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleSettingsRevisionAndGenerationRevocation(t *testing.T) {
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
	c, err := s.Configuration(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.Acquire(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope.Release()
	next, err := s.SavePolicy(e.ID, PolicyRequest{ExpectedRevision: c.Policy.Revision, Skills: []string{}, Connectors: []ConnectorScope{}, ConnectPorts: []int{443}})
	if err != nil {
		t.Fatal(err)
	}
	if scope.Check() == nil || scope.Context.Err() == nil || next.Entry.Generation == scope.Generation {
		t.Fatal("old generation not revoked")
	}
	if _, err = s.SavePolicy(e.ID, PolicyRequest{ExpectedRevision: c.Policy.Revision, Skills: []string{}, Connectors: []ConnectorScope{}}); err == nil {
		t.Fatal("accepted stale policy")
	}
	if _, err = s.SavePolicy(e.ID, PolicyRequest{ExpectedRevision: next.Policy.Revision, Skills: []string{}, Connectors: []ConnectorScope{}, ConnectPorts: []int{0}}); err == nil {
		t.Fatal("accepted invalid port")
	}
	if _, err = s.SavePolicy(e.ID, PolicyRequest{ExpectedRevision: next.Policy.Revision, AgentKey: "other"}); err == nil {
		t.Fatal("changed package policy")
	}
	if _, err = s.SaveUserConfig(e.ID, UserConfigRequest{ExpectedRevision: c.ConfigRevision, Values: map[string]any{"unknown": "value"}}); err == nil {
		t.Fatal("accepted unknown config")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.policy.ConnectPorts[0] != 443 {
		t.Fatal("lost port policy")
	}
}
func TestSingleInvalidManagedRecordsFailClosed(t *testing.T) {
	for _, which := range []string{"install.json", "policy.json", "user-config.json"} {
		t.Run(which, func(t *testing.T) {
			o := singleFixture(t)
			s, err := Open(o)
			if err != nil {
				t.Fatal(err)
			}
			e := s.Current()
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(o.Root, e.ID, which), []byte("null"), 0600); err != nil {
				t.Fatal(err)
			}
			s, err = Open(o)
			if err == nil {
				defer s.Close()
				if _, err = s.Configuration(e.ID); err == nil {
					t.Fatal("accepted invalid record")
				}
			}
		})
	}
}
func TestSingleRequiresExplicitPackageAndDoesNotScanInstallations(t *testing.T) {
	o := singleFixture(t)
	o.SingleRoot = ""
	if s, err := Open(o); err == nil {
		s.Close()
		t.Fatal("accepted application collection root")
	}
}
