package signal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// signal-cli keeps the account's keys and database under data/ in its
// directory, and attachments, avatars and stickers beside it. say carries
// data/ between nodes in the bridge's state: a copy is taken whenever
// signal-cli stops, and a node that takes the bridge over restores it, so
// say stays the same linked device instead of linking again.
//
// The copy is as of signal-cli's last stop: a node that dies leaves the
// one before, and the next node goes on from there. Sessions that moved
// on meanwhile are repaired by Signal as it does after a restored backup.
//
// Only say's own directory is carried. A data directory set in the config
// may hold other accounts, and stays on its node.

const (
	// dataSub is signal-cli's subdirectory with the account's keys.
	dataSub = "data"
	// dataIDFile, in the bridge's directory, names the copy its data/
	// matches.
	dataIDFile = "say-data-id"
	// maxData bounds a compressed copy, which is kept in the bridge's row.
	maxData = 16 << 20
	// saveTimeout bounds saving a copy after the bridge was stopped.
	saveTimeout = 10 * time.Second
)

// carries reports whether signal-cli's data moves with the bridge.
func (c *conn) carries() bool { return c.cfg.DataDir == "" }

// restore puts the saved copy of signal-cli's data in dir, unless dir
// already has it or something newer.
func (c *conn) restore(dir string) error {
	c.mu.Lock()
	data, id := c.st.Data, c.st.DataID
	c.mu.Unlock()
	if !c.carries() || len(data) == 0 {
		return nil
	}
	if have, _ := os.ReadFile(filepath.Join(dir, dataIDFile)); string(have) == id {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(dir, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unpack(data, tmp); err != nil {
		return fmt.Errorf("restore signal-cli's data: %w", err)
	}
	target := filepath.Join(dir, dataSub)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, dataIDFile), []byte(id), 0o600)
}

// keep saves a copy of signal-cli's data in dir to the bridge's state.
// signal-cli must not be running.
func (c *conn) keep(ctx context.Context, dir string) error {
	c.mu.Lock()
	account, old := c.st.Account, c.st.Data
	c.mu.Unlock()
	if !c.carries() || account == "" {
		return nil
	}
	data, err := pack(filepath.Join(dir, dataSub))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if bytes.Equal(data, old) {
		return nil
	}
	if len(data) > maxData {
		return fmt.Errorf("signal-cli's data is %d MB, too much to keep with the bridge; it stays on this node", len(data)>>20)
	}
	var b [8]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	c.mu.Lock()
	c.st.Data, c.st.DataID = data, id
	c.mu.Unlock()
	// The bridge may be stopping: the copy is saved all the same, before
	// another node can take the bridge.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	if err := c.save(sctx); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, dataIDFile), []byte(id), 0o600)
}

// keepOrWarn is keep, for when its failure leaves the data on this node
// only.
func (c *conn) keepOrWarn(ctx context.Context, dir string) {
	if err := c.keep(ctx, dir); err != nil {
		slog.Warn("signal: keeping signal-cli's data failed", "bridge", c.id, "err", err)
	}
}

// pack returns dir's files as a gzipped tar.
func pack(dir string) ([]byte, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o700})
		case !d.Type().IsRegular():
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: info.Size()}); err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unpack writes a gzipped tar made by pack into dir.
func unpack(data []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("bad name %q", h.Name)
		}
		path := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("bad entry %q", h.Name)
		}
	}
}
