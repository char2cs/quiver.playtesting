// Package web embeds the static browser client served by the gateway.
package web

import "embed"

//go:embed all:static
var FS embed.FS
