package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html style.css app.js
var webFiles embed.FS

// Handler returns an http.Handler that serves embedded web dashboard assets
func Handler() http.Handler {
	sub, err := fs.Sub(webFiles, ".")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
