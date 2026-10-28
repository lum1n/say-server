package matrix

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lum1n/say-server/internal/bridge"
)

// Files live in the homeserver's media repository, named by mxc:// URIs:
// a file message is uploaded first, then sent as an event naming it.

// fileInfo is the info of a file message. Numbers are floats, as some
// clients send them.
type fileInfo struct {
	Mimetype string  `json:"mimetype"`
	Size     float64 `json:"size"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
}

// partTxn marks the transaction ids of the events after the first of a
// message sent as several, so their echoes are known as say's own.
const partTxn = ".say-part-"

// isFile reports whether a msgtype carries a file.
func isFile(msgType string) bool {
	switch msgType {
	case "m.image", "m.video", "m.audio", "m.file":
		return true
	}
	return false
}

// files returns the file a message carries in the clear, as media. An
// encrypted one has no url, and stays a note in the text.
func (c *conn) files(cn content) []bridge.Media {
	server, id, ok := parseMXC(cn.URL)
	if !isFile(cn.MsgType) || !ok {
		return nil
	}
	var info fileInfo
	if cn.Info != nil {
		info = *cn.Info
	}
	return []bridge.Media{{
		Mime: info.Mimetype, Name: cmp.Or(cn.FileName, strings.TrimSpace(cn.Body)),
		Size: int64(info.Size), Width: int(info.W), Height: int(info.H),
		Open: func(ctx context.Context) (io.ReadCloser, error) { return c.download(ctx, server, id) },
	}}
}

// parseMXC splits mxc://server/id.
func parseMXC(u string) (server, id string, ok bool) {
	rest, ok := strings.CutPrefix(u, "mxc://")
	if !ok {
		return "", "", false
	}
	server, id, _ = strings.Cut(rest, "/")
	return server, id, server != "" && id != "" && !strings.Contains(id, "/")
}

// download fetches a file through the authenticated media API, or the
// old open one on a homeserver that lacks it.
func (c *conn) download(ctx context.Context, server, id string) (io.ReadCloser, error) {
	tail := "/download/" + url.PathEscape(server) + "/" + url.PathEscape(id)
	r, err := c.get(ctx, "/_matrix/client/v1/media"+tail)
	var e *apiError
	if errors.As(err, &e) && (e.Code == "M_UNRECOGNIZED" || e.Code == "" && e.Status == http.StatusNotFound || e.Status == http.StatusMethodNotAllowed) {
		r, err = c.get(ctx, "/_matrix/media/v3"+tail)
	}
	return r, err
}

// get starts reading an answer that is not JSON.
func (c *conn) get(ctx context.Context, p string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Homeserver+p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.roundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e, _ := answerError(resp, data)
		return nil, e
	}
	return resp.Body, nil
}

// upload puts a file in the media repository and returns the event
// content that sends it.
func (c *conn) upload(ctx context.Context, f bridge.Media) (map[string]any, error) {
	r, err := f.Open(ctx)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, err
	}
	name := cmp.Or(f.Name, "file")
	mime := cmp.Or(f.Mime, "application/octet-stream")
	var resp struct {
		URI string `json:"content_uri"`
	}
	err = c.doRaw(ctx, http.MethodPost, "/_matrix/media/v3/upload", url.Values{"filename": {name}}, mime, data, &resp)
	if isRefusal(err) {
		// Too big for the homeserver, or a type it won't keep.
		return nil, bridge.Rejected(err)
	}
	if err != nil {
		return nil, err
	}
	if resp.URI == "" {
		return nil, fmt.Errorf("matrix: the homeserver gave no content uri")
	}
	info := map[string]any{"mimetype": mime, "size": len(data)}
	if f.Width > 0 && f.Height > 0 {
		info["w"], info["h"] = f.Width, f.Height
	}
	return map[string]any{"msgtype": msgType(mime), "body": name, "filename": name, "url": resp.URI, "info": info}, nil
}

// msgType is the msgtype that shows a file of a type.
func msgType(mime string) string {
	kind, _, _ := strings.Cut(mime, "/")
	switch kind {
	case "image", "video", "audio":
		return "m." + kind
	}
	return "m.file"
}
