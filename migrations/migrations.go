// Package migrations embeds the numbered SQL schema migrations applied by the
// store at startup. Files are named NNNN_description.sql and applied in order.
package migrations

import "embed"

// FS holds every *.sql migration file.
//
//go:embed *.sql
var FS embed.FS
