// Package migrations bevat de databasemigraties, per driver in een eigen map. De SQL blijft zo dicht
// mogelijk bij standaard-SQL; alleen driver-specifieke zaken (zoals auto-increment) verschillen.
package migrations

import "embed"

// FS bevat alle migratiebestanden.
//
//go:embed sqlite/*.sql
var FS embed.FS
