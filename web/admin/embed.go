// Package adminweb embeds the log dashboard page served by cmd/admin.
package adminweb

import "embed"

//go:embed index.html admin.js admin.css
var FS embed.FS
