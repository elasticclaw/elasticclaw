package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
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

func TestAIChatArtifactRootIsSeparate(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	if _, err := artifact.NewStoreFromHubConfig(context.Background(), s.dataDir, nil); err != nil {
		t.Fatal(err)
	}
	clawRoot := filepath.Join(s.dataDir, "artifacts")
	if _, err := os.Stat(filepath.Join(clawRoot, "blobs")); err != nil {
		t.Fatal(err)
	}
	chatRoot := s.aiChatDeps().ArtifactsDir
	if chatRoot == clawRoot || chatRoot != filepath.Join(s.dataDir, "ai_chat", "artifacts") {
		t.Fatalf("AI Chat root %q collides with claw storage or is not isolated", chatRoot)
	}
}
