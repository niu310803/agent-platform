// Package scriptstate keeps run-owned, in-memory proofs of complete writes. A
// Run may start with the proofs its Chat's earlier file tool results recorded;
// nothing here is persisted.
package scriptstate

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"

	"agent-platform/internal/pathutil"
)

type Owner struct{ Agent, Run, Environment string }

// Proof names the content an earlier Run of the same Chat wrote to a file.
type Proof struct{ Path, SHA256 string }
type entry struct {
	hash string
	size int64
}
type Scope struct {
	mu       sync.Mutex
	owner    Owner
	files    map[string]entry
	revision uint64
}

func New(owner Owner) *Scope           { return &Scope{owner: owner, files: make(map[string]entry)} }
func (s *Scope) Owns(owner Owner) bool { return s != nil && s.owner == owner }
func (s *Scope) Revision() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Record uses the bytes committed by FileTools, never a later observer's bytes.
// An edit can only advance an existing proof whose old content still matches.
func (s *Scope) Record(owner Owner, path string, content []byte, previousHash string, complete bool) {
	if s == nil || owner != s.owner {
		return
	}
	p, err := pathutil.Canonicalize(path)
	if err != nil {
		return
	}
	sum := sha256.Sum256(content)
	next := entry{hash: hex.EncodeToString(sum[:]), size: int64(len(content))}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.files[p.Key]
	if !complete && (!exists || old.hash != previousHash) {
		if exists {
			delete(s.files, p.Key)
			s.revision++
		}
		return
	}
	if !matches(p.Host, next) {
		delete(s.files, p.Key)
		s.revision++
		return
	}
	s.files[p.Key] = next
	s.revision++
}

// Inherit adds a proof recorded by an earlier Run of the same Chat. Only its
// hash is taken over; Matches compares the file's bytes before every use. A
// proof already held is kept, so callers pass the most recent first.
func (s *Scope) Inherit(p Proof) {
	if s == nil {
		return
	}
	c, err := pathutil.Canonicalize(p.Path)
	if err != nil {
		return
	}
	info, err := os.Stat(c.Host)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.files[c.Key]; exists {
		return
	}
	s.files[c.Key] = entry{hash: p.SHA256, size: info.Size()}
	s.revision++
}

func (s *Scope) Matches(owner Owner, path string) bool {
	if s == nil || owner != s.owner {
		return false
	}
	p, err := pathutil.Canonicalize(path)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want, ok := s.files[p.Key]
	if !ok {
		return false
	}
	if !matches(p.Host, want) {
		delete(s.files, p.Key)
		s.revision++
		return false
	}
	return true
}

// MatchesHash additionally verifies bytes inspected inside a mounted execution
// environment; a same-named host file is not proof of container content.
func (s *Scope) MatchesHash(owner Owner, path, hash string) bool {
	if !s.Matches(owner, path) {
		return false
	}
	p, err := pathutil.Canonicalize(path)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want, ok := s.files[p.Key]
	if ok && hash != "" && want.hash == hash {
		return true
	}
	if ok {
		delete(s.files, p.Key)
		s.revision++
	}
	return false
}

func matches(path string, want entry) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != want.size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, want.size+1))
	return err == nil && n == want.size && hex.EncodeToString(h.Sum(nil)) == want.hash
}
