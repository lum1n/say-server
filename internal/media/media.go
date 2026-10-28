// Package media keeps the files people send: images and other attachments
// (docs/adr/0019-media.md). The database holds what a file is and where it
// was sent; a Store holds its bytes under the media's id.
package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	// The image types say reads the size of.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// DefaultMax is the largest file say takes, when no other limit is set.
const DefaultMax = 25 << 20

var (
	// ErrNotFound is a file the store does not have.
	ErrNotFound = errors.New("media: no such file")
	// ErrTooLarge is a file over the limit.
	ErrTooLarge = errors.New("media: the file is too large")
	// ErrWrongType is a file that is not what it says it is, or a type say
	// does not keep.
	ErrWrongType = errors.New("media: the file's type does not match")
)

// File is a stored file, read from the start or from anywhere in it.
type File interface {
	io.ReadSeekCloser
}

// Store holds files' bytes by id.
type Store interface {
	// Put stores what r holds as id. A file that is already there is
	// replaced.
	Put(ctx context.Context, id string, r io.Reader) error
	Open(ctx context.Context, id string) (File, error)
	Delete(ctx context.Context, id string) error
}

// Disk keeps files in a directory, two levels deep by the id's start so
// no one directory grows too large. Nodes of a cluster need the same
// directory, shared, until media can live in object storage.
type Disk struct{ dir string }

// NewDisk keeps files under dir, which it makes if needed.
func NewDisk(dir string) (*Disk, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Disk{dir: dir}, nil
}

func (d *Disk) path(id string) (string, error) {
	if len(id) < 4 || strings.ContainsAny(id, `/\.`) {
		return "", ErrNotFound
	}
	return filepath.Join(d.dir, id[:2], id[2:4], id), nil
}

func (d *Disk) Put(_ context.Context, id string, r io.Reader) error {
	p, err := d.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func (d *Disk) Open(_ context.Context, id string) (File, error) {
	p, err := d.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (d *Disk) Delete(_ context.Context, id string) error {
	p, err := d.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Memory keeps files in memory, for tests and for servers given no
// directory.
type Memory struct {
	mu    sync.Mutex
	files map[string][]byte
}

func NewMemory() *Memory { return &Memory{files: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, id string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.files[id] = b
	m.mu.Unlock()
	return nil
}

func (m *Memory) Open(_ context.Context, id string) (File, error) {
	m.mu.Lock()
	b, ok := m.files[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return nopCloser{bytes.NewReader(b)}, nil
}

func (m *Memory) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	delete(m.files, id)
	m.mu.Unlock()
	return nil
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// Info is what say learns of a file as it takes it in.
type Info struct {
	Mime   string
	Size   int64
	Width  int
	Height int
	SHA256 string
}

// Images are the types say shows inline and reads the size of. Any other
// type is served as a download.
var Images = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// refused are types that would run as a page on say's origin if a
// browser opened them.
var refused = map[string]bool{
	"text/html": true, "application/xhtml+xml": true, "image/svg+xml": true,
	"text/xml": true, "application/xml": true, "text/javascript": true,
	"application/javascript": true,
}

// Ingest stores r as id in s, at most max bytes. It sniffs the type: an
// image must be the image type it states, and a stated type that is empty
// takes the sniffed one. Images get their width and height.
func Ingest(ctx context.Context, s Store, id string, r io.Reader, stated string, max int64) (Info, error) {
	if max <= 0 {
		max = DefaultMax
	}
	br := bufio.NewReaderSize(r, 512)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return Info{}, err
	}
	typ, err := checkType(stated, http.DetectContentType(head))
	if err != nil {
		return Info{}, err
	}
	h := sha256.New()
	lr := &limited{r: io.TeeReader(br, h), left: max}
	if err := s.Put(ctx, id, lr); err != nil {
		if lr.over {
			s.Delete(context.WithoutCancel(ctx), id)
			return Info{}, ErrTooLarge
		}
		return Info{}, err
	}
	if lr.over {
		s.Delete(context.WithoutCancel(ctx), id)
		return Info{}, ErrTooLarge
	}
	info := Info{Mime: typ, Size: max - lr.left, SHA256: hex.EncodeToString(h.Sum(nil))}
	if Images[typ] {
		f, err := s.Open(ctx, id)
		if err != nil {
			return Info{}, err
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			s.Delete(context.WithoutCancel(ctx), id)
			return Info{}, fmt.Errorf("%w: %s that can't be read", ErrWrongType, typ)
		}
		info.Width, info.Height = cfg.Width, cfg.Height
	}
	return info, nil
}

// checkType returns the type to keep a file as.
func checkType(stated, sniffed string) (string, error) {
	sniffed, _, _ = mime.ParseMediaType(sniffed)
	if stated == "" {
		stated = sniffed
	}
	stated, _, err := mime.ParseMediaType(stated)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a media type", ErrWrongType, stated)
	}
	if stated == "image/jpg" {
		stated = "image/jpeg"
	}
	if refused[stated] || refused[sniffed] {
		return "", fmt.Errorf("%w: say does not keep %s", ErrWrongType, stated)
	}
	if (Images[stated] || Images[sniffed]) && stated != sniffed {
		return "", fmt.Errorf("%w: the file is %s, not %s", ErrWrongType, sniffed, stated)
	}
	return stated, nil
}

// limited reads at most left bytes, and fails past them.
type limited struct {
	r    io.Reader
	left int64
	over bool
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left < 0 {
		l.over = true
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		l.over = true
		return n, ErrTooLarge
	}
	return n, err
}

// Sign returns the query of a URL that serves media id without a token
// until exp: exp=<unix seconds>&sig=<hex>.
func Sign(key []byte, id string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return "exp=" + e + "&sig=" + mac(key, id, e)
}

// Verify checks a signed URL's exp and sig for id.
func Verify(key []byte, id, exp, sig string) bool {
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(mac(key, id, exp)))
}

func mac(key []byte, id, exp string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("media\x00" + id + "\x00" + exp))
	return hex.EncodeToString(m.Sum(nil))
}

// Expiry is when a URL signed now stops working: at least ttl from now,
// and the same for half of ttl at a time, so a URL stays the same long
// enough for clients to cache what it serves.
func Expiry(now time.Time, ttl time.Duration) time.Time {
	step := ttl / 2
	return now.Truncate(step).Add(ttl + step)
}
