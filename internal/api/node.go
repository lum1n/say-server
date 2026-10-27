package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/update"
	"github.com/lum1n/say-server/say/release"
)

// nodeInfo is GET /api/v1/node: this node's version and, on a node that
// updates itself, the updater's last run (docs/adr/0017-own-node.md).
type nodeInfo struct {
	Version string      `json:"version"`
	Update  *updateInfo `json:"update"`
}

type updateInfo struct {
	State       string     `json:"state,omitempty"`
	Error       string     `json:"error,omitempty"`
	Latest      string     `json:"latest,omitempty"`
	Available   bool       `json:"available"`
	Requested   bool       `json:"requested"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	UpdatedFrom string     `json:"updated_from,omitempty"`
}

func (s *Server) nodeInfo() (nodeInfo, error) {
	out := nodeInfo{Version: s.version}
	if s.updateDir == "" {
		return out, nil
	}
	st, err := update.ReadStatus(filepath.Join(s.updateDir, update.StatusFile))
	if err != nil {
		return out, err
	}
	_, err = os.Stat(filepath.Join(s.updateDir, update.RequestFile))
	out.Update = &updateInfo{
		State:       st.State,
		Error:       st.Error,
		Latest:      st.Latest,
		Available:   release.Newer(st.Latest, s.version),
		Requested:   err == nil,
		UpdatedFrom: st.UpdatedFrom,
	}
	if !st.CheckedAt.IsZero() {
		out.Update.CheckedAt = &st.CheckedAt
	}
	if !st.UpdatedAt.IsZero() {
		out.Update.UpdatedAt = &st.UpdatedAt
	}
	return out, nil
}

func requireNodeAdmin(me core.Principal) error {
	if !me.Admin {
		return errorf(http.StatusForbidden, "forbidden", "only instance admins see and update the node")
	}
	return nil
}

func (s *Server) getNode(w http.ResponseWriter, _ *http.Request, me core.Principal) {
	if err := requireNodeAdmin(me); err != nil {
		writeError(w, err)
		return
	}
	info, err := s.nodeInfo()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// updateNode asks the updater to check for a release now and install it.
// say runs unprivileged, so it only leaves a request file; systemd starts
// the updater when it appears (say-update.path).
func (s *Server) updateNode(w http.ResponseWriter, _ *http.Request, me core.Principal) {
	if err := requireNodeAdmin(me); err != nil {
		writeError(w, err)
		return
	}
	if s.updateDir == "" {
		writeError(w, errorf(http.StatusConflict, "no_updates", "this node doesn't update itself; update it the way it was installed"))
		return
	}
	if err := os.WriteFile(filepath.Join(s.updateDir, update.RequestFile), nil, 0o644); err != nil {
		writeError(w, err)
		return
	}
	info, err := s.nodeInfo()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}
