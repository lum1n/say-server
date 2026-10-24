package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestServer() *httptest.Server {
	s := New("test")
	s.assets = fstest.MapFS{
		shell:           {Data: []byte("<html>shell</html>")},
		"widget.js":     {Data: []byte("widget")},
		"assets/app.js": {Data: []byte("app")},
	}
	return httptest.NewServer(s.Handler())
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header
}

func TestHealth(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	code, body, _ := get(t, ts.URL+"/api/v1/health")
	var h struct{ Status, Version string }
	if code != 200 || json.Unmarshal([]byte(body), &h) != nil || h.Status != "ok" || h.Version != "test" {
		t.Fatalf("health = %d %q", code, body)
	}
}

func TestApp(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	cases := []struct {
		path string
		code int
		body string
	}{
		{"/", 200, "shell"},
		{"/embed/pk_123", 200, "shell"},
		{"/widget.js", 200, "widget"},
		{"/assets/app.js", 200, "app"},
		{"/assets/missing.js", 404, ""},
		{"/api/v1/nope", 404, ""},
	}
	for _, c := range cases {
		code, body, _ := get(t, ts.URL+c.path)
		if code != c.code || !strings.Contains(body, c.body) {
			t.Errorf("GET %s = %d %q, want %d containing %q", c.path, code, body, c.code, c.body)
		}
	}
	if _, _, h := get(t, ts.URL+"/assets/app.js"); !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Errorf("assets not cached: %q", h.Get("Cache-Control"))
	}
}

func TestPlaceholderWithoutBuild(t *testing.T) {
	s := New("test")
	s.assets = fstest.MapFS{}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if code, body, _ := get(t, ts.URL+"/"); code != 200 || !strings.Contains(body, "make build") {
		t.Fatalf("placeholder = %d %q", code, body)
	}
}
