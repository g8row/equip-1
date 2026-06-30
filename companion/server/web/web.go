// Package web embeds the built React SPA (web/dist). The real build overwrites
// dist/ later; for now it contains a placeholder index.html so the embed
// compiles.
package web

import "embed"

// DistFS holds the embedded contents of the dist/ directory.
//
//go:embed dist
var DistFS embed.FS
