package api

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	PlanFree  = 0
	PlanBasic = 1
	PlanPlus  = 2
	PlanPro   = 3
)

var loginRe = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

func normalizeLogin(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// PremiumUser — JSON 6.1 (field names are part of the contract).
type PremiumUser struct {
	ID                   uuid.UUID `json:"id"`
	Login                string    `json:"login"`
	FullName             string    `json:"full_name"`
	Description          string    `json:"description"`
	PhoneNumber          string    `json:"phone_number"`
	PremiumPlanTypeIndex int16     `json:"premium_plan_type_index"`
	StartAt              Time      `json:"start_at"`
	ExpiryAt             Time      `json:"expiry_at"`
	IsActive             bool      `json:"is_active"`
	CreatedAt            Time      `json:"created_at"`
	MaxDevices           *int16    `json:"max_devices"`
	CredentialsUpdatedAt Time      `json:"credentials_updated_at"`
	SignupSource         string    `json:"signup_source"`
}

const userCols = `id, login, full_name, description, phone_number, premium_plan_type_index,
	start_at, expiry_at, is_active, created_at, max_devices, credentials_updated_at, signup_source`

func scanUser(row pgx.Row) (*PremiumUser, error) {
	var u PremiumUser
	err := row.Scan(&u.ID, &u.Login, &u.FullName, &u.Description, &u.PhoneNumber, &u.PremiumPlanTypeIndex,
		&u.StartAt.Time, &u.ExpiryAt.Time, &u.IsActive, &u.CreatedAt.Time, &u.MaxDevices,
		&u.CredentialsUpdatedAt.Time, &u.SignupSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return &u, err
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getUser(ctx context.Context, q querier, id uuid.UUID) (*PremiumUser, error) {
	return scanUser(q.QueryRow(ctx, `select `+userCols+` from premium_users where id = $1`, id))
}

// canWriteCloud — 3.2: is_active AND expiry_at > now() AND plan = PRO.
func (u *PremiumUser) writeCheck(now time.Time) error {
	if !u.IsActive {
		return errAccountBlocked
	}
	if u.PremiumPlanTypeIndex != PlanPro || !u.ExpiryAt.After(now) {
		return errForbiddenNotPro
	}
	return nil
}

// loginTaken reports whether a login is already used.
func loginTaken(ctx context.Context, q querier, login string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `select exists(select 1 from premium_users where login = $1)`, login).Scan(&exists)
	return exists, err
}
