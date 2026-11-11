package app

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"time"

	"agent-platform/internal/config"
)

// A per-start local RS256 owner uses the existing verifier and launch-ticket
// flow. The private key never leaves memory. The host reads owner.json directly.
func prepareSingleOwner(cfg *config.Config) error {
	if cfg.Auth.JWKSURI != "" {
		return nil
	}
	dir := filepath.Join(cfg.Paths.StateDir, "webapp-owner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	file := filepath.Join(dir, "public.pem")
	if err = os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0600); err != nil {
		return err
	}
	subject := "webapp-owner"
	audience := base64.RawURLEncoding.EncodeToString(pub[len(pub)-24:])
	cfg.Auth = config.AuthConfig{Enabled: true, LocalPublicKeyFile: file, Issuer: "agent-platform-single", Audience: audience, AllowedSubjects: []string{subject}, RequireExpiry: true}
	expires := time.Now().Add(30 * 24 * time.Hour).Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"sub": subject, "aud": audience, "iss": cfg.Auth.Issuer, "exp": expires, "iat": time.Now().Unix()})
	input := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"token": input + "." + base64.RawURLEncoding.EncodeToString(sig), "expiresAt": expires * 1000, "subject": subject})
	path := filepath.Join(dir, "owner.json")
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
