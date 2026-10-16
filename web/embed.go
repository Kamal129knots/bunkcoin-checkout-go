// Package web holds the static frontend (HTML/CSS/JS), embedded into the binary.
// Edit the files here; `make run` picks up changes on restart.
package web

import "embed"

//go:embed *.html *.css *.js
var FS embed.FS
