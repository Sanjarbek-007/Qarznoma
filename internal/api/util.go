package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxSmallBody = 64 << 10
	maxSyncBody  = 16 << 20 // 500 notes with JSON content can be large
)

// bindJSON decodes the body into v. Unknown fields are ignored (client may send extra ones).
func bindJSON(c *gin.Context, v any, limit int64) error {
	body := http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return newErr(http.StatusRequestEntityTooLarge, "VALIDATION", "so'rov juda katta")
		}
		if errors.Is(err, io.EOF) {
			return validation("so'rov tanasi bo'sh")
		}
		return validation("noto'g'ri JSON: " + err.Error())
	}
	return nil
}

// Time is a timestamptz that accepts "2026-11-17 12:42:00+00" and ISO-8601, and is
// rendered as ISO-8601 UTC ("2026-11-17T12:42:00Z") — HANDOFF 2.
type Time struct{ time.Time }

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02T15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("noto'g'ri sana: %q", s)
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p, err := ParseTime(s)
	if err != nil {
		return err
	}
	t.Time = p
	return nil
}

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}

// NullTime renders as null when invalid.
type NullTime struct {
	Time  time.Time
	Valid bool
}

func (t NullTime) MarshalJSON() ([]byte, error) {
	if !t.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time.UTC().Format(time.RFC3339Nano))
}

func (t *NullTime) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Valid = false
		return nil
	}
	var tt Time
	if err := tt.UnmarshalJSON(b); err != nil {
		return err
	}
	t.Time, t.Valid = tt.Time, true
	return nil
}

func isUniqueViolation(err error, constraintSubstr string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && strings.Contains(pe.ConstraintName, constraintSubstr)
}

func isFKViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

// inTx runs fn inside a transaction.
func (s *Server) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, fn)
}

func clientIP(c *gin.Context) string { return c.ClientIP() }
