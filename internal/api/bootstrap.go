package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Qarznoma/qarznoma-backend/internal/auth"
)

// EnsureAdmin creates the bootstrap admin from ADMIN_LOGIN/ADMIN_PASSWORD if that login doesn't
// exist yet. An existing account is never modified (its password is not reset from env).
func (s *Server) EnsureAdmin(ctx context.Context) error {
	if s.cfg.AdminLogin == "" {
		return nil
	}
	login := normalizeLogin(s.cfg.AdminLogin)
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		var role string
		err := tx.QueryRow(ctx, `select p.id, coalesce(a.role, '') from premium_users p
			left join auth_credentials a on a.user_id = p.id where p.login = $1`, login).Scan(&id, &role)
		if err == nil {
			if role != "admin" {
				slog.Warn("ADMIN_LOGIN exists but is not an admin; leaving it unchanged", "login", login)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		hash, err := auth.HashPassword(s.cfg.AdminPassword)
		if err != nil {
			return err
		}
		id = uuid.New()
		if _, err = tx.Exec(ctx, `
			insert into premium_users (id, login, full_name, premium_plan_type_index, expiry_at, signup_source)
			values ($1, $2, 'Admin', 0, $3, 'ADMIN')`, id, login, time.Now().AddDate(100, 0, 0)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `insert into auth_credentials (user_id, password_hash, role) values ($1, $2, 'admin')`,
			id, hash); err != nil {
			return err
		}
		slog.Info("bootstrap admin created", "login", login)
		return nil
	})
}
