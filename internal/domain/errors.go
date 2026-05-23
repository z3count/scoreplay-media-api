// Package domain — errors.go defines sentinel errors for the domain layer.
//
// These errors are used by the service layer to communicate business rule
// violations. The HTTP handler layer maps them to appropriate HTTP status codes.
//
// Using sentinel errors (rather than error strings) allows reliable error
// checking with errors.Is() in handler code, decoupling the error semantics
// from the transport layer.
package domain

import "errors"

var (
	// ErrNotFound is returned when a requested entity does not exist.
	// Mapped to HTTP 404.
	ErrNotFound = errors.New("entity not found")

	// ErrConflict is returned when an operation would violate a uniqueness
	// constraint (e.g., creating a tag with a name that already exists, though
	// in practice we use idempotent upserts for tags).
	// Mapped to HTTP 409.
	ErrConflict = errors.New("entity already exists")

	// ErrValidation is returned when input fails business rule validation
	// (empty name, name too long, invalid media type, etc.).
	// Mapped to HTTP 400.
	ErrValidation = errors.New("validation error")

	// ErrUnsupportedMediaType is returned when the uploaded file's detected
	// content type is not an allowed image or video format.
	// Mapped to HTTP 415.
	ErrUnsupportedMediaType = errors.New("unsupported media type")

	// ErrFileTooLarge is returned when the uploaded file exceeds the configured
	// maximum size limit.
	// Mapped to HTTP 413.
	ErrFileTooLarge = errors.New("file too large")
)
