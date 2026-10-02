// Package web embeds the dashboard so the hub ships as a single binary.
package web

import "embed"

//go:embed index.html app.js tiles.js style.css admin.html admin.js sw.js manifest.webmanifest icon.svg icon-192.png icon-512.png icon-maskable-512.png apple-touch-icon.png
var FS embed.FS
