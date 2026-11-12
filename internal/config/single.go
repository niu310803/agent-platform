package config

import "net"

// Single hosting is an assembly profile, independent of Agent mode.
func (c *Config) configureSingle() {
	c.ExcludedTools = append(c.ExcludedTools, "automation_manage", "automation_query", "chat_send", "chat_notify", "chat_submit", "kbase_library_manage", "kbase_library_query", "kbase_list", "kbase_search", "kbase_files", "kbase_read", "kbase_status", "speech_generate", "desktop_kanban", "desktop_market", "desktop_service", "desktop_settings", "desktop_shell", "desktop_site", "desktop_webapp", "surface_activate", "surface_browser", "surface_click", "surface_close", "surface_dialog", "surface_downloads", "surface_element", "surface_list", "surface_login", "surface_navigate", "surface_observe", "surface_open", "surface_screenshot", "surface_scroll", "surface_state", "surface_upload", "surface_viewport", "_skill_candidate_write_", "_skill_candidate_list_")
	if host, _, err := net.SplitHostPort(c.ListenAddress); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			c.WebApp.Frontend.ListenAddress = "0.0.0.0"
		}
	}
	c.Automation.Enabled = false
	c.Memory.Enabled = false
	c.Voice.Enabled = false
	c.Gateways = nil
	c.Channels = nil
	c.ContainerHub.Enabled = false
	c.FileTools.Hooks.AfterFileChange.LSPDiagnostics.Enabled = false
}

func (c Config) SingleWebApp() bool { return c.WebAppHost == WebAppHostSingle }
