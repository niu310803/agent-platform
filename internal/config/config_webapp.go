package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

type WebAppHostMode string

const (
	WebAppHostDisabled WebAppHostMode = "disabled"
	WebAppHostSingle   WebAppHostMode = "single"
)

func ParseWebAppHostMode(value string) (WebAppHostMode, error) {
	switch WebAppHostMode(strings.TrimSpace(value)) {
	case "", WebAppHostDisabled:
		return WebAppHostDisabled, nil
	case WebAppHostSingle:
		return WebAppHostMode(strings.TrimSpace(value)), nil
	default:
		return "", fmt.Errorf("webapp-host must be disabled or single")
	}
}

// WebAppConfig is deployment policy, never application-controlled settings.
// JSON tags match the strict runtime YAML keys; public DTOs are separate.
type WebAppConfig struct {
	PlatformOrigin string               `json:"-"`
	Backend        WebAppBackendConfig  `json:"backend"`
	Frontend       WebAppFrontendConfig `json:"frontend"`
	Logs           WebAppLogConfig      `json:"logs"`
}

type WebAppFrontendConfig struct {
	ListenAddress string `json:"listen-address"`
	Port          int    `json:"port"`
}

type WebAppBackendConfig struct {
	Port                  int                 `json:"port"`
	MaxOldSpaceSizeMB     int                 `json:"max-old-space-size-mb"`
	RSSStopMB             int                 `json:"rss-stop-mb"`
	HealthIntervalSeconds int                 `json:"health-interval-seconds"`
	UnhealthyThreshold    int                 `json:"unhealthy-threshold"`
	Restart               WebAppRestartConfig `json:"restart"`
}

type WebAppRestartConfig struct {
	MaxAttempts        int `json:"max-attempts"`
	WindowSeconds      int `json:"window-seconds"`
	StableResetSeconds int `json:"stable-reset-seconds"`
}

type WebAppLogConfig struct {
	MaxSizeMB int `json:"max-size-mb"`
	MaxFiles  int `json:"max-files"`
}

func DefaultWebAppConfig() WebAppConfig {
	return WebAppConfig{
		Frontend: WebAppFrontendConfig{ListenAddress: "127.0.0.1"},
		Backend: WebAppBackendConfig{
			MaxOldSpaceSizeMB: 256, RSSStopMB: 512,
			HealthIntervalSeconds: 10, UnhealthyThreshold: 3,
			Restart: WebAppRestartConfig{MaxAttempts: 5, WindowSeconds: 600, StableResetSeconds: 300},
		}, Logs: WebAppLogConfig{MaxSizeMB: 10, MaxFiles: 3},
	}
}

func (c *Config) applyWebApp(raw any) error {
	if err := checkWebAppKeys(raw, reflect.TypeFor[WebAppConfig](), "webapp"); err != nil {
		return err
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("webapp: %w", err)
	}
	// Decode into a copy so a malformed section cannot partially change policy.
	next := c.WebApp
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return fmt.Errorf("webapp: %w", err)
	}
	if err := next.validateLimits(); err != nil {
		return err
	}
	c.WebApp = next
	return nil
}

// encoding/json alone accepts case-insensitive keys and null primitives.
// Validate the original YAML shapes first, including nested exact key spelling.
func checkWebAppKeys(raw any, typ reflect.Type, path string) error {
	values, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a mapping", path)
	}
	fields := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if key := field.Tag.Get("json"); key != "-" {
			fields[key] = field.Type
		}
	}
	for k, v := range values {
		ft, ok := fields[k]
		if !ok {
			return fmt.Errorf("unknown %s.%s", path, k)
		}
		if v == nil {
			return fmt.Errorf("%s.%s must not be null", path, k)
		}
		switch ft.Kind() {
		case reflect.Bool:
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s.%s must be boolean", path, k)
			}
		case reflect.String:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s.%s must be a string", path, k)
			}
		case reflect.Int:
			if _, ok := v.(int64); !ok {
				return fmt.Errorf("%s.%s must be an integer", path, k)
			}
		}
		if ft.Kind() == reflect.Struct {
			if err := checkWebAppKeys(v, ft, path+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c WebAppConfig) validateLimits() error {
	for _, value := range []struct {
		name   string
		n, max int
	}{
		{"backend.max-old-space-size-mb", c.Backend.MaxOldSpaceSizeMB, 65536},
		{"backend.rss-stop-mb", c.Backend.RSSStopMB, 131072},
		{"backend.health-interval-seconds", c.Backend.HealthIntervalSeconds, 300},
		{"backend.unhealthy-threshold", c.Backend.UnhealthyThreshold, 100},
		{"backend.restart.max-attempts", c.Backend.Restart.MaxAttempts, 100},
		{"backend.restart.window-seconds", c.Backend.Restart.WindowSeconds, 86400},
		{"backend.restart.stable-reset-seconds", c.Backend.Restart.StableResetSeconds, 86400},
		{"logs.max-size-mb", c.Logs.MaxSizeMB, 1024}, {"logs.max-files", c.Logs.MaxFiles, 20},
	} {
		if value.n < 1 || value.n > value.max {
			return fmt.Errorf("webapp.%s must be between 1 and %d", value.name, value.max)
		}
	}
	f, b := c.Frontend, c.Backend
	if f.ListenAddress != "127.0.0.1" && f.ListenAddress != "0.0.0.0" {
		return fmt.Errorf("webapp.frontend.listen-address must be 127.0.0.1 or 0.0.0.0")
	}
	for _, port := range []int{f.Port, b.Port} {
		if port != 0 && (port < 1024 || port > 65535) {
			return fmt.Errorf("webapp ports must be zero or unprivileged ports")
		}
	}
	if f.Port != 0 && f.Port == b.Port {
		return fmt.Errorf("webapp frontend and backend ports must differ")
	}
	if b.RSSStopMB <= b.MaxOldSpaceSizeMB {
		return fmt.Errorf("webapp.backend.rss-stop-mb must exceed max-old-space-size-mb")
	}
	return nil
}

func (c *Config) validateWebApp(goos string) error {
	if c.WebAppHost == WebAppHostSingle {
		if c.RuntimeMode != RuntimeModeStandalone || (goos != "linux" && goos != "darwin" && goos != "windows") {
			return fmt.Errorf("single WebApp requires standalone on Linux, macOS or Windows")
		}
		if c.Auth.JWKSURI != "" && (!c.Auth.Enabled || !c.Auth.RequireExpiry || c.Auth.Issuer == "" || c.Auth.Audience == "" || len(c.Auth.AllowedSubjects) != 1 || c.Auth.AllowedSubjects[0] == "") {
			return fmt.Errorf("single WebApp JWKS requires issuer, audience, expiry and one owner")
		}
		return c.WebApp.validateLimits()
	}
	return nil
}

func (c Config) LegacyWebAppsCodeDir() string { return c.Paths.LegacyWebAppsDir }
func (c Config) WebAppDataDir() string        { return c.Paths.WebAppDataDir }
