// Package ui embeds the compiled frontend (ui/dist) into the binary.
package ui

import "embed"

//go:embed all:dist
var Dist embed.FS
