package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
)

func TestAIChatArtifactStaticExportPaths(t *testing.T) {
	s := &Server{}
	mux := http.NewServeMux()
	mux.Handle("GET /artifacts/{slug}/raw", aichat.RawArtifactRoutes(aichat.Deps{}))
	s.serveWebUI(mux, fstest.MapFS{"index.html": {Data: []byte("home")}, "artifacts/index.html": {Data: []byte("artifact shell")}})
	for _, path := range []string{"/artifacts/meal-swap-abcdef", "/artifacts/meal-swap-abcdef/v/2"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || rec.Body.String() != "artifact shell" {
			t.Fatal(path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/meal-swap-abcdef/raw", nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}
