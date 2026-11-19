package api

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// colKind describes how a JSON field maps to a Postgres column.
type colKind int

const (
	kUUID      colKind = iota // uuid not null
	kUUIDNull                 // uuid null
	kText                     // text not null
	kTextNull                 // text null
	kInt64                    // bigint not null
	kInt64Null                // bigint null
	kInt16                    // smallint not null
	kBool                     // boolean not null
	kTime                     // timestamptz not null (accepts both formats, renders ISO Z)
)

type colSpec struct {
	name     string
	kind     colKind
	required bool // missing/null => 422 (otherwise def is used)
	def      any
	maxLen   int      // text: max runes (0 = no limit)
	enum     []string // text: allowed values
	pattern  *regexp.Regexp
	min, max int64 // ints: inclusive range when max > min
}

// syncTable describes one syncable table (5.4).
type syncTable struct {
	name string
	// cols the client may write (in DB column order). "id" is always first.
	cols []colSpec
	// owned: table has premium_user_id (all except debts).
	owned bool
	// conflict target for upsert.
	conflict string
	// server-managed output columns (not writable): returned on PULL.
	serverCols []colSpec
	// idIsUUID: keyset after_id is parsed as uuid (false for finance_categories).
	idIsUUID bool
	// plan-limit key for 7 ("debtors" / "debts"), "" = no limit.
	limitKind string
}

const (
	maxName = 200
	maxText = 5000
	maxKey  = 64
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

func col(name string, kind colKind, opts ...func(*colSpec)) colSpec {
	c := colSpec{name: name, kind: kind}
	switch kind {
	case kText:
		c.def = ""
	case kInt64, kInt16:
		c.def = int64(0)
	case kBool:
		c.def = false
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func req(c *colSpec)                          { c.required = true }
func maxLen(n int) func(*colSpec)             { return func(c *colSpec) { c.maxLen = n } }
func enum(v ...string) func(*colSpec)         { return func(c *colSpec) { c.enum = v } }
func def(v any) func(*colSpec)                { return func(c *colSpec) { c.def = v } }
func rng(lo, hi int64) func(*colSpec)         { return func(c *colSpec) { c.min, c.max = lo, hi } }
func pattern(r *regexp.Regexp) func(*colSpec) { return func(c *colSpec) { c.pattern = r } }

var (
	colID          = col("id", kUUID, req)
	colIsDeleted   = col("is_deleted", kBool)
	colUpdatedAtCl = col("updated_at_client", kInt64)
	colOwner       = col("premium_user_id", kUUID)
	colLastUpdated = col("last_updated", kInt64)
)

var syncTables = map[string]*syncTable{
	"debtors": {
		name: "debtors", owned: true, conflict: "(id)", idIsUUID: true, limitKind: "debtors",
		cols: []colSpec{
			colID,
			col("full_name", kText, req, maxLen(maxName)),
			col("description", kText, maxLen(maxText)),
			col("phone_number", kText, maxLen(maxKey)),
			col("currency_type", kInt16, rng(0, 4)),
			col("repayment_date", kTime, req),
			colIsDeleted,
			col("updated_at_client", kInt64Null),
		},
		serverCols: []colSpec{colOwner, col("total_debts_amount", kInt64), colLastUpdated},
	},
	"debts": {
		name: "debts", owned: false, conflict: "(id)", idIsUUID: true, limitKind: "debts",
		cols: []colSpec{
			colID,
			col("debtor_id", kUUID, req),
			col("debt_amount", kInt64, req),
			col("debt_description", kText, maxLen(maxText)),
			col("debt_date", kTime, req),
			colIsDeleted,
		},
		serverCols: []colSpec{colLastUpdated},
	},
	"finance_categories": {
		name: "finance_categories", owned: true, conflict: "(premium_user_id, id)", idIsUUID: false,
		cols: []colSpec{
			col("id", kText, req, maxLen(maxKey)),
			col("label", kText, maxLen(maxName)),
			col("type", kText, req, enum("INCOME", "EXPENSE")),
			col("color_key", kText, req, maxLen(maxKey)),
			col("icon_key", kText, req, maxLen(maxKey)),
			col("is_hidden", kBool),
			colIsDeleted,
			colUpdatedAtCl,
		},
		serverCols: []colSpec{colOwner, colLastUpdated},
	},
	"finance_tags": {
		name: "finance_tags", owned: true, conflict: "(id)", idIsUUID: true,
		cols: []colSpec{
			colID,
			col("name", kText, req, maxLen(maxName)),
			col("color_key", kText, req, maxLen(maxKey)),
			colIsDeleted,
			colUpdatedAtCl,
		},
		serverCols: []colSpec{colOwner, colLastUpdated},
	},
	"finance_goals": {
		name: "finance_goals", owned: true, conflict: "(id)", idIsUUID: true,
		cols: []colSpec{
			colID,
			col("name", kText, req, maxLen(maxName)),
			col("note", kText, maxLen(maxText)),
			col("currency", kText, req, pattern(currencyRe)),
			col("target_amount", kInt64, req),
			col("saved_amount", kInt64),
			col("color_key", kText, req, maxLen(maxKey)),
			col("icon_key", kText, req, maxLen(maxKey)),
			col("deadline", kTextNull, maxLen(maxKey)),
			colIsDeleted,
			colUpdatedAtCl,
		},
		serverCols: []colSpec{colOwner, colLastUpdated},
	},
	"finance_transactions": {
		name: "finance_transactions", owned: true, conflict: "(id)", idIsUUID: true,
		cols: []colSpec{
			colID,
			col("type", kText, req, enum("INCOME", "EXPENSE", "CONVERSION")),
			col("currency", kText, req, pattern(currencyRe)),
			col("amount", kInt64, req, rng(0, math.MaxInt64)),
			col("to_currency", kTextNull, pattern(currencyRe)),
			col("to_amount", kInt64Null),
			col("rate", kTextNull, maxLen(maxKey)),
			col("category_id", kText, maxLen(maxKey)),
			col("description", kText, maxLen(maxText)),
			col("tag_ids", kText, maxLen(maxText)),
			col("tx_date", kText, req, maxLen(maxKey)),
			col("linked_debt_id", kUUIDNull),
			col("is_opening_balance", kBool),
			colIsDeleted,
			colUpdatedAtCl,
		},
		serverCols: []colSpec{colOwner, colLastUpdated},
	},
	"notes": {
		name: "notes", owned: true, conflict: "(id)", idIsUUID: true,
		cols: []colSpec{
			colID,
			col("title", kText, maxLen(maxText)),
			col("content", kText, maxLen(1<<20)),
			col("color_key", kText, def("DEFAULT"), maxLen(maxKey)),
			col("labels", kText, maxLen(maxText)),
			col("is_pinned", kBool),
			col("is_archived", kBool),
			col("reminder_at", kInt64Null),
			colIsDeleted,
			colUpdatedAtCl,
		},
		serverCols: []colSpec{colOwner, colLastUpdated},
	},
}

// decodeValue converts a raw JSON value into the Go value pgx expects for the column.
func (c colSpec) decodeValue(raw json.RawMessage, present bool) (any, error) {
	isNull := !present || string(raw) == "null"
	if isNull {
		if c.required {
			return nil, fmt.Errorf("%s majburiy", c.name)
		}
		switch c.kind {
		case kUUIDNull, kTextNull, kInt64Null:
			return nil, nil
		}
		if c.def != nil {
			if c.kind == kInt16 {
				return int16(c.def.(int64)), nil
			}
			return c.def, nil
		}
		return nil, fmt.Errorf("%s majburiy", c.name)
	}
	switch c.kind {
	case kUUID, kUUIDNull:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: matn (uuid) kutilgan", c.name)
		}
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%s: noto'g'ri uuid", c.name)
		}
		return u, nil
	case kText, kTextNull:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: matn kutilgan", c.name)
		}
		if strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("%s: NUL belgisi mumkin emas", c.name)
		}
		if c.maxLen > 0 && len([]rune(s)) > c.maxLen {
			return nil, fmt.Errorf("%s: %d belgidan oshmasin", c.name, c.maxLen)
		}
		if len(c.enum) > 0 && !contains(c.enum, s) {
			return nil, fmt.Errorf("%s: ruxsat etilgan qiymatlar %v", c.name, c.enum)
		}
		if c.pattern != nil && !c.pattern.MatchString(s) {
			return nil, fmt.Errorf("%s: noto'g'ri qiymat", c.name)
		}
		return s, nil
	case kInt64, kInt64Null, kInt16:
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("%s: butun son kutilgan", c.name)
		}
		if c.max > c.min && (n < c.min || n > c.max) {
			return nil, fmt.Errorf("%s: %d..%d oralig'ida bo'lsin", c.name, c.min, c.max)
		}
		if c.kind == kInt16 {
			if n < math.MinInt16 || n > math.MaxInt16 {
				return nil, fmt.Errorf("%s: juda katta", c.name)
			}
			return int16(n), nil
		}
		return n, nil
	case kBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s: true/false kutilgan", c.name)
		}
		return b, nil
	case kTime:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: sana matni kutilgan", c.name)
		}
		t, err := ParseTime(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", c.name, err)
		}
		return t, nil
	}
	return nil, fmt.Errorf("%s: unknown kind", c.name)
}

// scanTarget returns a pointer to scan the column into and a func producing the JSON value.
func (c colSpec) scanTarget() (any, func() any) {
	switch c.kind {
	case kUUID:
		var v uuid.UUID
		return &v, func() any { return v }
	case kUUIDNull:
		var v *uuid.UUID
		return &v, func() any { return v }
	case kText:
		var v string
		return &v, func() any { return v }
	case kTextNull:
		var v *string
		return &v, func() any { return v }
	case kInt64:
		var v int64
		return &v, func() any { return v }
	case kInt64Null:
		var v *int64
		return &v, func() any { return v }
	case kInt16:
		var v int16
		return &v, func() any { return v }
	case kBool:
		var v bool
		return &v, func() any { return v }
	case kTime:
		var v time.Time
		return &v, func() any { return v.UTC().Format(time.RFC3339Nano) }
	}
	panic("unknown kind")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
