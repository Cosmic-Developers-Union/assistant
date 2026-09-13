package status

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeGitea(t *testing.T) {
	gitea := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/version" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"version":"1.27.0"}`))
	}))
	defer gitea.Close()
	if !ProbeGitea(t.Context(), gitea.URL) {
		t.Error("Gitea 站点应探测为 true")
	}

	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer github.Close()
	if ProbeGitea(t.Context(), github.URL) {
		t.Error("404 站点应探测为 false")
	}
}
