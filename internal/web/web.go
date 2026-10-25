// Package web embeds the single-page UI.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var staticFiles embed.FS

// Handler serves the embedded files at the root path.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
