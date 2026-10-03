// Package web embeds the app so the hub ships as a single binary.
package web

import "embed"

//go:embed *.html *.js *.css manifest.webmanifest *.svg *.png pages/*.js
var FS embed.FS
