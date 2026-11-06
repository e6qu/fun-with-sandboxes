package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A link's name, /<parent>/<alias>, reaches a container route whole, as
// Docker's router takes it.
func TestLinkReferences(t *testing.T) {
	mux := http.NewServeMux()
	for _, p := range []string{"GET /containers/{id}/json", "GET /containers/{id}/attach/ws", "POST /containers/create", "GET /containers/json"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(p + " " + r.PathValue("id"))) })
	}
	for path, want := range map[string]string{
		"/containers/web/database/json":      "GET /containers/{id}/json web/database",
		"/containers/web/json":               "GET /containers/{id}/json web",
		"/containers/web/database/attach/ws": "GET /containers/{id}/attach/ws web/database",
		"/containers/json":                   "GET /containers/json ",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		linkReference(r.URL)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if got := w.Body.String(); got != want {
			t.Errorf("%s routes to %q, want %q", path, got, want)
		}
	}
}
