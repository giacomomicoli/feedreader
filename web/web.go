// Package web embeds the HTML templates and static assets so the binary is
// self-contained. Handlers live in internal/web.
package web

import "embed"

// Templates holds web/templates/*.html.
//
//go:embed templates
var Templates embed.FS

// Static holds web/static (htmx, CSS), served under /static/.
//
//go:embed static
var Static embed.FS
