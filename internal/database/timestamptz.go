package database

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Timestamptz converts a UTC time to a nullable timestamptz value. The zero
// time maps to NULL, which is the convention every repository uses for
// "unset" timestamps.
func Timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

// TimestamptzPtr is Timestamptz for an optional time; nil maps to NULL.
func TimestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return Timestamptz(*t)
}

// TimePtr converts a nullable timestamptz to an optional time.
func TimePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	return &ts.Time
}
