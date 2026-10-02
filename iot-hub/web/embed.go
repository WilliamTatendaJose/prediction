// Package web embeds the dashboard so the hub ships as a single binary.
package web

import "embed"

//go:embed index.html app.js tiles.js style.css
var FS embed.FS
