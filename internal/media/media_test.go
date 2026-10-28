package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

var gif = []byte("GIF89a\x02\x00\x03\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

func TestIngest(t *testing.T) {
	ctx := context.Background()
	for _, s := range []Store{NewMemory(), must(NewDisk(t.TempDir()))} {
		info, err := Ingest(ctx, s, "0001img", bytes.NewReader(gif), "", 0)
		if err != nil || info.Mime != "image/gif" || info.Width != 2 || info.Height != 3 || info.Size != int64(len(gif)) || info.SHA256 == "" {
			t.Fatalf("%T gif = %+v, %v", s, info, err)
		}
		f, err := s.Open(ctx, "0001img")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(f)
		f.Close()
		if !bytes.Equal(got, gif) {
			t.Fatalf("%T read back %q", s, got)
		}

		for _, c := range []struct{ data, typ string }{
			{string(gif), "image/png"},    // not what it says
			{"<html><p>hi", "text/plain"}, // sniffed as a page
			{"hi", "text/html"},
			{"<svg/>", "image/svg+xml"},
			{"not a gif", "image/gif"},
		} {
			if _, err := Ingest(ctx, s, "0002bad", strings.NewReader(c.data), c.typ, 0); !errors.Is(err, ErrWrongType) {
				t.Errorf("%T %q as %s: %v", s, c.data, c.typ, err)
			}
		}
		if _, err := Ingest(ctx, s, "0003big", strings.NewReader("abcdef"), "text/plain", 5); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%T over the limit: %v", s, err)
		}
		if _, err := s.Open(ctx, "0003big"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%T kept a file over the limit: %v", s, err)
		}
		if info, err := Ingest(ctx, s, "0004txt", strings.NewReader("hello"), "text/plain; charset=utf-8", 5); err != nil || info.Mime != "text/plain" {
			t.Errorf("%T text at the limit = %+v, %v", s, info, err)
		}
	}
}

func TestDiskRefusesPaths(t *testing.T) {
	d := must(NewDisk(t.TempDir()))
	if err := d.Put(context.Background(), "../../etc", strings.NewReader("x")); err == nil {
		t.Fatal("a path as id was taken")
	}
}

func TestSign(t *testing.T) {
	key := []byte("k")
	now := time.Now()
	exp := Expiry(now, 24*time.Hour)
	if exp.Before(now.Add(24*time.Hour)) || exp.After(now.Add(36*time.Hour)) {
		t.Fatalf("expiry %v from %v", exp, now)
	}
	if Expiry(now.Add(time.Minute), 24*time.Hour) != exp && Expiry(now.Add(-time.Minute), 24*time.Hour) != exp {
		t.Fatal("the expiry changes every minute")
	}
	q := Sign(key, "m1", exp)
	var e, sig string
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k == "exp" {
			e = v
		} else {
			sig = v
		}
	}
	if !Verify(key, "m1", e, sig) || Verify(key, "m2", e, sig) || Verify([]byte("other"), "m1", e, sig) {
		t.Fatal("verify")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
