package api

import (
	_ "embed"
	"net/http"
)

// uiHTML is the self-contained admin console. It talks to this same server's
// /admin, /engine and /data APIs using the tenant API key the operator enters.
//
//go:embed ui/index.html
var uiHTML []byte

func (s *Server) ui(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	_, _ = w.Write(uiHTML)
}
