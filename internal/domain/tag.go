package domain

import (
	"time"

	"github.com/google/uuid"
)

// Tag represents a label that can be attached to one or more media items.
//
// Tags model a many-to-many relationship with media through the media_tags
// junction table. A tag can be anything: a player's name, a location, a
// competition, a date label, etc.
//
// Key invariants:
//   - Name is unique across the system (enforced at DB level). This enables
//     idempotent creation: creating a tag with an existing name returns the
//     existing tag instead of failing.
//   - Name is trimmed of leading/trailing whitespace before storage.
//   - Name has a maximum length of 255 characters.
type Tag struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}
