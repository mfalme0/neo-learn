// Package dbfs exposes the SQL migration files to Go code.
package dbfs

import "embed"

// Migrations holds the versioned schema.
//
// Embedded rather than read from disk so tests and tooling apply the same
// schema the deploy pipeline does, with no dependency on the working
// directory or on the migrate binary being installed.
//
//go:embed migrations/*.sql
var Migrations embed.FS