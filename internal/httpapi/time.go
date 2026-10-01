package httpapi

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// pgTime converts a nullable Postgres timestamp to a nullable Go time.
//
// A zero/invalid pgtype maps to nil so the JSON field is omitted entirely
// rather than emitted as "0001-01-01T00:00:00Z", which is not a real
// completion time and would read as one in the UI.
func pgTime(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
