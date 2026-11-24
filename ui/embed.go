// Package ui embeds the built web UI. ui/dist/.keep lets Go build without a UI build.
package ui

import "embed"

// Dist holds the contents of ui/dist.
//
//go:embed all:dist
var Dist embed.FS
