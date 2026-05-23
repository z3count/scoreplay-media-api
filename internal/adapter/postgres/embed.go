// Package postgres provides an embedded filesystem containing the SQL migration
// files. By embedding them here (in the same package as the migration directory),
// we avoid path resolution issues with go:embed's relative path constraint.
package postgres

import "embed"

// Migrations contains the embedded SQL migration files.
// They are bundled into the binary at compile time, ensuring migrations are
// always in sync with the application version.
//
//go:embed migrations/*.sql
var Migrations embed.FS
