// Package web embeds the browser client so the server binary is self-contained.
package web

import "embed"

//go:embed index.html app.js owd.js chart.js style.css
var FS embed.FS
