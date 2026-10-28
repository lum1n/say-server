package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/lum1n/say-server/internal/store/db"
)

// Media is an uploaded file: what it is, and the room it was sent to.
type Media struct {
	ID         string
	RoomID     string
	UploaderID string
	Mime       string
	Name       string
	Size       int64
	// Width and Height are an image's size in pixels; 0 for other files.
	Width     int
	Height    int
	SHA256    string
	CreatedAt time.Time
}

func (s *sqlStore) CreateMedia(ctx context.Context, m Media) error {
	return s.q().CreateMedia(ctx, db.CreateMediaParams{
		ID:         m.ID,
		RoomID:     m.RoomID,
		UploaderID: m.UploaderID,
		Mime:       m.Mime,
		Name:       m.Name,
		Size:       m.Size,
		Width:      int32(m.Width),
		Height:     int32(m.Height),
		Sha256:     m.SHA256,
		CreatedAt:  millis(m.CreatedAt),
	})
}

func (s *sqlStore) GetMedia(ctx context.Context, id string) (Media, error) {
	row, err := s.q().GetMedia(ctx, id)
	if err != nil {
		return Media{}, notFound(err)
	}
	return Media{
		ID:         row.ID,
		RoomID:     row.RoomID,
		UploaderID: row.UploaderID,
		Mime:       row.Mime,
		Name:       row.Name,
		Size:       row.Size,
		Width:      int(row.Width),
		Height:     int(row.Height),
		SHA256:     row.Sha256,
		CreatedAt:  time.UnixMilli(row.CreatedAt),
	}, nil
}

func (s *sqlStore) InstanceKey(ctx context.Context, name string) ([]byte, error) {
	b := make([]byte, 32)
	rand.Read(b)
	// The first node to ask wins; the others read its key.
	if err := s.q().ClaimInstanceValue(ctx, db.ClaimInstanceValueParams{Key: "key:" + name, Value: hex.EncodeToString(b)}); err != nil {
		return nil, err
	}
	v, err := s.q().InstanceValue(ctx, "key:"+name)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}
