package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// Cursor format: base64url (no padding) over a compact JSON payload.
//
// Why JSON: trivial to extend with new fields and human-debuggable if needed
// (a developer can base64-decode a cursor and read it).
// Why base64url: safe in query strings and Location headers without escaping.
//
// Cursors are part of the API contract but are opaque to clients — they
// should pass them back verbatim. The exact encoding is an implementation
// detail and may change without notice.

type tagCursorPayload struct {
	N string    `json:"n"`
	I uuid.UUID `json:"i"`
}

type mediaCursorPayload struct {
	T time.Time `json:"t"`
	I uuid.UUID `json:"i"`
}

// EncodeTagCursor produces the opaque string representation of a tag cursor.
// A nil cursor encodes to the empty string (used to signal "no more pages").
func EncodeTagCursor(c *port.TagCursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(tagCursorPayload{N: c.Name, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeTagCursor parses an opaque cursor string back into a TagCursor.
// An empty string decodes to a nil cursor (meaning "start from the beginning").
// Malformed input returns a wrapped domain.ErrValidation so the HTTP layer can
// map it to 400 Bad Request.
func DecodeTagCursor(s string) (*port.TagCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cursor encoding", domain.ErrValidation)
	}
	var p tagCursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%w: invalid cursor payload", domain.ErrValidation)
	}
	return &port.TagCursor{Name: p.N, ID: p.I}, nil
}

// EncodeMediaCursor produces the opaque string representation of a media cursor.
// A nil cursor encodes to the empty string.
func EncodeMediaCursor(c *port.MediaCursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(mediaCursorPayload{T: c.CreatedAt, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeMediaCursor parses an opaque cursor string back into a MediaCursor.
// An empty string decodes to a nil cursor. Malformed input returns a wrapped
// domain.ErrValidation.
func DecodeMediaCursor(s string) (*port.MediaCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cursor encoding", domain.ErrValidation)
	}
	var p mediaCursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%w: invalid cursor payload", domain.ErrValidation)
	}
	return &port.MediaCursor{CreatedAt: p.T, ID: p.I}, nil
}
