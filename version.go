package platform

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var version string

func Version() string { return strings.TrimPrefix(strings.TrimSpace(version), "v") }
