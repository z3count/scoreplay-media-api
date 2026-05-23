package service

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

func TestTagCursor_RoundTrip(t *testing.T) {
	orig := &port.TagCursor{Name: "Mbappé", ID: uuid.New()}
	encoded := EncodeTagCursor(orig)
	if encoded == "" {
		t.Fatal("encoded cursor should not be empty")
	}

	got, err := DecodeTagCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil cursor")
	}
	if got.Name != orig.Name {
		t.Errorf("Name: got %q want %q", got.Name, orig.Name)
	}
	if got.ID != orig.ID {
		t.Errorf("ID: got %s want %s", got.ID, orig.ID)
	}
}

func TestTagCursor_Empty(t *testing.T) {
	if s := EncodeTagCursor(nil); s != "" {
		t.Errorf("nil cursor should encode to empty string, got %q", s)
	}
	c, err := DecodeTagCursor("")
	if err != nil {
		t.Fatalf("empty string should decode without error, got %v", err)
	}
	if c != nil {
		t.Errorf("empty string should decode to nil cursor, got %+v", c)
	}
}

func TestTagCursor_Malformed(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"invalid base64", "!!!not-base64!!!"},
		{"valid base64, invalid JSON", "Zm9v"}, // base64("foo")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeTagCursor(tt.input)
			if err == nil {
				t.Fatalf("expected error for input %q", tt.input)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestMediaCursor_RoundTrip(t *testing.T) {
	orig := &port.MediaCursor{
		CreatedAt: time.Date(2026, 5, 21, 12, 0, 0, 123456789, time.UTC),
		ID:        uuid.New(),
	}
	encoded := EncodeMediaCursor(orig)
	if encoded == "" {
		t.Fatal("encoded cursor should not be empty")
	}

	got, err := DecodeMediaCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("CreatedAt: got %v want %v", got.CreatedAt, orig.CreatedAt)
	}
	if got.ID != orig.ID {
		t.Errorf("ID: got %s want %s", got.ID, orig.ID)
	}
}

func TestMediaCursor_Empty(t *testing.T) {
	if s := EncodeMediaCursor(nil); s != "" {
		t.Errorf("nil cursor should encode to empty string, got %q", s)
	}
	c, err := DecodeMediaCursor("")
	if err != nil {
		t.Fatalf("empty string should decode without error, got %v", err)
	}
	if c != nil {
		t.Errorf("empty string should decode to nil cursor, got %+v", c)
	}
}

func TestMediaCursor_Malformed(t *testing.T) {
	_, err := DecodeMediaCursor("!!!not-base64!!!")
	if err == nil {
		t.Fatal("expected error for malformed cursor")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}

	// Valid base64 but not valid JSON.
	_, err = DecodeMediaCursor("YWJjZGVm")
	if err == nil {
		t.Fatal("expected JSON unmarshal error to surface as ErrValidation")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
}
