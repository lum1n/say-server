package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/update"
)

func TestNode(t *testing.T) {
	e := newEnv(t)
	_, admin := e.admin("Ada")
	_, bob := e.user("Bob", core.PrincipalHuman)
	e.srv.version = "0.4.0"

	var info nodeInfo
	if code := e.do("GET", "/node", bob, nil, nil); code != 403 {
		t.Fatalf("GET /node as a user = %d", code)
	}
	if code := e.do("GET", "/node", admin, nil, &info); code != 200 || info.Version != "0.4.0" || info.Update != nil {
		t.Fatalf("GET /node without updates = %d %+v", code, info)
	}
	if code := e.do("POST", "/node/update", admin, nil, nil); code != 409 {
		t.Fatalf("POST /node/update without updates = %d", code)
	}

	dir := t.TempDir()
	e.srv.updateDir = dir
	if code := e.do("GET", "/node", admin, nil, &info); code != 200 || info.Update == nil || info.Update.Available || info.Update.Requested {
		t.Fatalf("GET /node before a check = %d %+v", code, info.Update)
	}
	checked := time.Now().UTC().Truncate(time.Second)
	b, _ := json.Marshal(update.Status{State: update.StateOK, Latest: "0.4.1", CheckedAt: checked})
	os.WriteFile(filepath.Join(dir, update.StatusFile), b, 0o644)
	info = nodeInfo{}
	if code := e.do("GET", "/node", admin, nil, &info); code != 200 || !info.Update.Available || info.Update.Latest != "0.4.1" ||
		info.Update.CheckedAt == nil || !info.Update.CheckedAt.Equal(checked) || info.Update.UpdatedAt != nil {
		t.Fatalf("GET /node after a check = %d %+v", code, info.Update)
	}
	if code := e.do("POST", "/node/update", bob, nil, nil); code != 403 {
		t.Fatalf("POST /node/update as a user = %d", code)
	}
	if code := e.do("POST", "/node/update", admin, nil, &info); code != 202 || !info.Update.Requested {
		t.Fatalf("POST /node/update = %d %+v", code, info.Update)
	}
	if _, err := os.Stat(filepath.Join(dir, update.RequestFile)); err != nil {
		t.Fatal("no request file:", err)
	}
}
