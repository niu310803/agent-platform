package scriptstate

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestProofLifecycleAndIsolation(t *testing.T) {
	owner := Owner{"ordinary", "run-1", "host"}
	scope := New(owner)
	path := filepath.Join(t.TempDir(), "script")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("first")
	scope.Record(owner, path, []byte("first"), "", false)
	if scope.Matches(owner, path) {
		t.Fatal("partial edit created proof")
	}
	scope.Record(owner, path, []byte("first"), "", true)
	if !scope.Matches(owner, path) {
		t.Fatal("complete write missing proof")
	}
	for _, other := range []Owner{{"other", "run-1", "host"}, {"ordinary", "run-2", "host"}, {"ordinary", "run-1", "container"}} {
		if scope.Matches(other, path) {
			t.Fatal("proof crossed owner boundary")
		}
	}
	write("second")
	scope.Record(owner, path, []byte("second"), fmt.Sprintf("%x", sha256.Sum256([]byte("first"))), false)
	if !scope.Matches(owner, path) {
		t.Fatal("authored edit lost proof")
	}
	revision := scope.Revision()
	write("outsider")
	if scope.Matches(owner, path) || scope.Revision() <= revision {
		t.Fatal("external mutation did not revoke")
	}
	write("second")
	if scope.Matches(owner, path) {
		t.Fatal("revoked proof came back")
	}
	scope.Record(owner, path, []byte("not actually written"), "", true)
	if scope.Matches(owner, path) {
		t.Fatal("failed write created proof")
	}
}

func TestProofSymlinkAndConcurrentRecords(t *testing.T) {
	owner := Owner{"agent", "run", "host"}
	scope := New(owner)
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join(dir, fmt.Sprint(i))
			b := []byte(fmt.Sprint(i))
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Error(err)
				return
			}
			scope.Record(owner, p, b, "", true)
			if !scope.Matches(owner, p) {
				t.Error("missing proof")
			}
			scope.Revision()
		}(i)
	}
	wg.Wait()
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(filepath.Join(dir, "0"), alias); err != nil {
		t.Skip(err)
	}
	if !scope.Matches(owner, alias) {
		t.Fatal("canonical alias not recognized")
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.WriteFile(foreign, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, alias); err != nil {
		t.Fatal(err)
	}
	if scope.Matches(owner, alias) {
		t.Fatal("symlink target inherited old proof")
	}
}

func TestGuestContentMismatchRevokesProof(t *testing.T) {
	owner := Owner{"agent", "run", "container"}
	scope := New(owner)
	p := filepath.Join(t.TempDir(), "script")
	data := []byte("echo original")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	scope.Record(owner, p, data, "", true)
	if scope.MatchesHash(owner, p, "foreign") || scope.Matches(owner, p) {
		t.Fatal("guest mutation did not revoke the environment's proof")
	}
}

func TestInheritedProofKeepsMostRecentAndRechecksBytes(t *testing.T) {
	owner := Owner{"ordinary", "run-2", "host"}
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	scope := New(owner)
	scope.Inherit(Proof{Path: path, SHA256: hash("current")})
	scope.Inherit(Proof{Path: path, SHA256: hash("older")}) // an older record of the same file
	scope.Inherit(Proof{Path: filepath.Join(filepath.Dir(path), "missing"), SHA256: hash("x")})
	if !scope.Matches(owner, path) || scope.Matches(Owner{"ordinary", "run-1", "host"}, path) {
		t.Fatal("inherited proof must belong to this Run and keep the most recent hash")
	}
	// An inherited proof advances and is revoked exactly like a recorded one.
	if err := os.WriteFile(path, []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	scope.Record(owner, path, []byte("edited"), hash("current"), false)
	if !scope.Matches(owner, path) {
		t.Fatal("edit of inherited content lost its proof")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if scope.Matches(owner, path) {
		t.Fatal("changed bytes kept the inherited proof")
	}
	stale := New(owner)
	stale.Inherit(Proof{Path: path, SHA256: hash("current")})
	if stale.Matches(owner, path) {
		t.Fatal("hash that no longer matches the file was accepted")
	}
}
