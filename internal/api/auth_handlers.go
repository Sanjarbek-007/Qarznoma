package api

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Qarznoma/qarznoma-backend/internal/auth"
)

// AuthResponse — 6.10, identical for login/register/google/refresh.
type AuthResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	UserID       uuid.UUID `json:"user_id"`
}

const refreshGrace = 60 * time.Second

// issueSession creates a refresh token row and an access JWT.
func (s *Server) issueSession(ctx context.Context, tx pgx.Tx, uid uuid.UUID, deviceID *string) (*AuthResponse, error) {
	var credsAt time.Time
	var role string
	err := tx.QueryRow(ctx, `
		select p.credentials_updated_at, coalesce(a.role, 'user')
		  from premium_users p left join auth_credentials a on a.user_id = p.id where p.id = $1`, uid).
		Scan(&credsAt, &role)
	if err != nil {
		return nil, err
	}
	tok, hash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `
		insert into refresh_tokens (id, user_id, token_hash, device_id, expires_at)
		values ($1, $2, $3, $4, $5)`,
		uuid.New(), uid, hash, deviceID, time.Now().Add(s.cfg.RefreshTTL)); err != nil {
		return nil, err
	}
	access, err := s.tokens.Issue(uid, role, credsAt)
	if err != nil {
		return nil, err
	}
	return &AuthResponse{
		AccessToken: access, RefreshToken: tok,
		ExpiresIn: int(s.tokens.TTL().Seconds()), UserID: uid,
	}, nil
}

func optionalDeviceID(c *gin.Context) *string {
	if d := strings.TrimSpace(c.GetHeader("X-Device-ID")); d != "" && len(d) <= 128 {
		return &d
	}
	return nil
}

// POST /v1/auth/login
func (s *Server) login(c *gin.Context) {
	var req struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	login := normalizeLogin(req.Login)
	if !s.rlLogin.allow(clientIP(c) + "|" + login) {
		fail(c, errRateLimited)
		return
	}
	ctx := c.Request.Context()
	uid, err := s.checkCredentials(ctx, login, req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	var resp *AuthResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var e error
		resp, e = s.issueSession(ctx, tx, uid, optionalDeviceID(c))
		return e
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// checkCredentials returns the user id for a valid login/password or INVALID_CREDENTIALS.
func (s *Server) checkCredentials(ctx context.Context, login, password string) (uuid.UUID, error) {
	var uid uuid.UUID
	var hash string
	err := s.db.QueryRow(ctx, `
		select p.id, a.password_hash from premium_users p
		  join auth_credentials a on a.user_id = p.id where p.login = $1`, login).Scan(&uid, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.BurnPasswordCheck(password)
		return uuid.Nil, errInvalidCredentials
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !auth.CheckPassword(hash, password) {
		return uuid.Nil, errInvalidCredentials
	}
	return uid, nil
}

// POST /v1/auth/refresh — rotates the refresh token.
func (s *Server) refresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if req.RefreshToken == "" {
		fail(c, errUnauthorized)
		return
	}
	ctx := c.Request.Context()
	var resp *AuthResponse
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id, uid uuid.UUID
		var deviceID *string
		var expiresAt time.Time
		var revokedAt *time.Time
		var rotated bool
		err := tx.QueryRow(ctx, `
			select id, user_id, device_id, expires_at, revoked_at, rotated
			  from refresh_tokens where token_hash = $1 for update`,
			auth.HashRefreshToken(req.RefreshToken)).
			Scan(&id, &uid, &deviceID, &expiresAt, &revokedAt, &rotated)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnauthorized
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if !expiresAt.After(now) {
			return errUnauthorized
		}
		if revokedAt != nil && !(rotated && now.Sub(*revokedAt) < refreshGrace) {
			return errUnauthorized
		}
		if revokedAt == nil {
			if _, err = tx.Exec(ctx, `update refresh_tokens set revoked_at = now(), rotated = true where id = $1`, id); err != nil {
				return err
			}
		}
		resp, err = s.issueSession(ctx, tx, uid, deviceID)
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// POST /v1/auth/logout
func (s *Server) logout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if req.RefreshToken != "" {
		if _, err := s.db.Exec(c.Request.Context(), `
			update refresh_tokens set revoked_at = coalesce(revoked_at, now()), rotated = false
			 where token_hash = $1`, auth.HashRefreshToken(req.RefreshToken)); err != nil {
			fail(c, err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

// revokeUserAccess — 3.7: bump credentials_updated_at and kill every refresh token.
func revokeUserAccess(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		update premium_users set credentials_updated_at = greatest(now(), credentials_updated_at + interval '1 millisecond')
		 where id = $1`, uid); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		update refresh_tokens set revoked_at = coalesce(revoked_at, now()), rotated = false where user_id = $1`, uid)
	return err
}

// POST /v1/auth/register
func (s *Server) register(c *gin.Context) {
	var req struct {
		Login    string `json:"login"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if !s.rlRegister.allow(clientIP(c)) {
		fail(c, errRateLimited)
		return
	}
	login := normalizeLogin(req.Login)
	if !loginRe.MatchString(login) {
		fail(c, errInvalidLogin)
		return
	}
	if len([]rune(req.Password)) < 6 {
		fail(c, errWeakPassword)
		return
	}
	if len(req.Password) > 72 {
		fail(c, validation("parol 72 baytdan oshmasin"))
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	fullName := truncate(strings.TrimSpace(req.FullName), 100)
	if fullName == "" {
		fullName = login
	}
	ctx := c.Request.Context()
	var resp *AuthResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		uid := uuid.New()
		if _, err := tx.Exec(ctx, `
			insert into premium_users (id, login, full_name, premium_plan_type_index, start_at, expiry_at, is_active, signup_source)
			values ($1, $2, $3, 0, now(), now(), true, 'SELF')`, uid, login, fullName); err != nil {
			if isUniqueViolation(err, "login") {
				return errLoginTaken
			}
			return err
		}
		if _, err := tx.Exec(ctx, `insert into auth_credentials (user_id, password_hash) values ($1, $2)`, uid, hash); err != nil {
			return err
		}
		var e error
		resp, e = s.issueSession(ctx, tx, uid, optionalDeviceID(c))
		return e
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GET /v1/auth/login-available?login=
func (s *Server) loginAvailable(c *gin.Context) {
	if !s.rlLoginCheck.allow(clientIP(c)) {
		fail(c, errRateLimited)
		return
	}
	login := normalizeLogin(c.Query("login"))
	if !loginRe.MatchString(login) {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	taken, err := loginTaken(c.Request.Context(), s.db, login)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"available": !taken})
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// GET /v1/auth/suggest-login?base=
func (s *Server) suggestLogin(c *gin.Context) {
	if !s.rlLoginCheck.allow(clientIP(c)) {
		fail(c, errRateLimited)
		return
	}
	base := strings.ToLower(nonAlnum.ReplaceAllString(c.Query("base"), ""))
	base = truncate(base, 20)
	ctx := c.Request.Context()
	for i := 0; i < 25; i++ {
		var try string
		if len(base) >= 3 {
			try = base + "_" + randDigits(1000, 9999)
		} else {
			try = randDigits(100000000, 999999999)
		}
		taken, err := loginTaken(ctx, s.db, try)
		if err != nil {
			fail(c, err)
			return
		}
		if !taken {
			c.JSON(http.StatusOK, gin.H{"login": try})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"login": randDigits(100000000000, 999999999999)})
}

func randDigits(min, max int64) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(max-min+1))
	return big.NewInt(0).Add(n, big.NewInt(min)).String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
