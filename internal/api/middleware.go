package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	ctxRequestID = "request_id"
	ctxUserID    = "user_id"
	ctxRole      = "role"
)

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		c.Set(ctxRequestID, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// accessLog logs the route template only (no query strings, tokens, phone numbers — 9).
func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		attrs := []any{
			"method", c.Request.Method, "route", route, "status", c.Writer.Status(),
			"dur_ms", time.Since(start).Milliseconds(), "request_id", c.GetString(ctxRequestID),
		}
		if uid, ok := c.Get(ctxUserID); ok {
			attrs = append(attrs, "user_id", uid.(uuid.UUID).String())
		}
		slog.Info("http", attrs...)
	}
}

func recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic", "panic", r, "stack", string(debug.Stack()), "request_id", c.GetString(ctxRequestID))
				fail(c, errInternal)
			}
		}()
		c.Next()
	}
}

// cors allows only the configured web origin (delete-account page on another domain).
func (s *Server) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if s.cfg.WebOrigin != "" && origin == s.cfg.WebOrigin {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type")
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
		}
		c.Next()
	}
}

func bearer(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// authenticate validates the JWT and checks it against the DB: the user must still exist and
// credentials_updated_at must match the token (3.7 — admin password change kills old tokens).
func (s *Server) authenticate(c *gin.Context, tok string) error {
	claims, uid, err := s.tokens.Parse(tok)
	if err != nil {
		return errUnauthorized
	}
	var credsAt time.Time
	var role string
	err = s.db.QueryRow(c.Request.Context(), `
		select p.credentials_updated_at, coalesce(a.role, 'user')
		  from premium_users p left join auth_credentials a on a.user_id = p.id
		 where p.id = $1`, uid).Scan(&credsAt, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return errUnauthorized
	}
	if err != nil {
		return err
	}
	if credsAt.UnixMilli() != claims.CV {
		return errUnauthorized
	}
	c.Set(ctxUserID, uid)
	c.Set(ctxRole, role)
	return nil
}

func (s *Server) authRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := bearer(c)
		if tok == "" {
			fail(c, errUnauthorized)
			return
		}
		if err := s.authenticate(c, tok); err != nil {
			fail(c, err)
			return
		}
		c.Next()
	}
}

// authOptional: no token => anonymous; a token that is present but invalid => 401
// (so the client refreshes instead of silently getting the anonymous view).
func (s *Server) authOptional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if tok := bearer(c); tok != "" {
			if err := s.authenticate(c, tok); err != nil {
				fail(c, err)
				return
			}
		}
		c.Next()
	}
}

func (s *Server) adminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString(ctxRole) != "admin" {
			fail(c, errForbidden)
			return
		}
		c.Next()
	}
}

func (s *Server) syncRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.rlSync.allow(userID(c).String()) {
			fail(c, errRateLimited)
			return
		}
		c.Next()
	}
}

func userID(c *gin.Context) uuid.UUID {
	v, _ := c.Get(ctxUserID)
	id, _ := v.(uuid.UUID)
	return id
}
