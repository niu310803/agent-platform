package config

import (
	"reflect"
	"testing"
)

func TestWebAppHostRejectsRetiredCompose(t *testing.T) {
	for _, v := range []string{"compose", "multi", "desktop", "Single"} {
		if _, err := ParseWebAppHostMode(v); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	for _, v := range []string{"", "disabled", "single"} {
		if _, err := ParseWebAppHostMode(v); err != nil {
			t.Fatal(err)
		}
	}
}
func TestWebAppDeploymentBoundary(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		c := defaultConfig(LoadOptions{RuntimeMode: "standalone", WebAppHost: "single"})
		if err := c.validateWebApp(goos); err != nil {
			t.Fatal(err)
		}
		c.RuntimeMode = RuntimeModeDesktop
		if err := c.validateWebApp(goos); err == nil {
			t.Fatal("nested Desktop host accepted")
		}
	}
}
func TestWebAppStrictYAMLAndRetiredConfiguration(t *testing.T) {
	for _, raw := range []string{"webapps: {}", "webapps: {enabled: false}", "webapps: {enabled: true, max-running: 1}"} {
		c := defaultConfig(LoadOptions{})
		if err := c.applyRuntimeFile(configFixture(t, "runtime.yml", raw+"\n")); err == nil {
			t.Fatalf("accepted retired section %s", raw)
		}
	}
	for _, raw := range []string{"null", "[]", "{enabled: true}", "{max-running: 1}", "{content-domain: apps.example.com}", "{max-archive-mib: 1}", "{frontend: {port-start: 20200}}", "{frontend: {port: 80}}", "{logs: {max-files: 0}}", "{backend: {port: \"20000\"}}"} {
		c := defaultConfig(LoadOptions{})
		before := c.WebApp
		if err := c.applyRuntimeFile(configFixture(t, "runtime.yml", "webapp: "+raw+"\n")); err == nil {
			t.Fatalf("accepted invalid section %s", raw)
		}
		if !reflect.DeepEqual(before, c.WebApp) {
			t.Fatal("partial configuration update")
		}
	}
	c := defaultConfig(LoadOptions{})
	if err := c.applyRuntimeFile(configFixture(t, "runtime.yml", "webapp:\n  frontend:\n    port: 20200\n  backend:\n    port: 20000\n")); err != nil {
		t.Fatal(err)
	}
	if c.WebApp.Frontend.Port != 20200 || c.WebApp.Backend.Port != 20000 {
		t.Fatal("port configuration ignored")
	}
}
