package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/idtoken"
)

// GoogleIdentity is what we need from a verified Google ID token.
type GoogleIdentity struct {
	Sub, Email, Name, Nonce string
}

// GoogleVerifier verifies a Google ID token (signature, aud, exp). Interface for tests.
type GoogleVerifier interface {
	Verify(ctx context.Context, idToken string) (*GoogleIdentity, error)
}

type googleVerifier struct{ aud string }

func newGoogleVerifier(aud string) GoogleVerifier { return &googleVerifier{aud: aud} }

func (g *googleVerifier) Verify(ctx context.Context, tok string) (*GoogleIdentity, error) {
	p, err := idtoken.Validate(ctx, tok, g.aud)
	if err != nil {
		return nil, err
	}
	if iss := p.Issuer; iss != "accounts.google.com" && iss != "https://accounts.google.com" {
		return nil, errors.New("bad issuer")
	}
	str := func(k string) string { v, _ := p.Claims[k].(string); return v }
	return &GoogleIdentity{Sub: p.Subject, Email: str("email"), Name: str("name"), Nonce: str("nonce")}, nil
}

var loginCharsRe = regexp.MustCompile(`[^a-z0-9_]`)

// POST /v1/auth/google {"id_token","nonce","full_name"}
func (s *Server) googleSignIn(c *gin.Context) {
	if s.google == nil {
		fail(c, newErr(http.StatusNotImplemented, "NOT_CONFIGURED", "Google bilan kirish sozlanmagan"))
		return
	}
	if !s.rlGoogle.allow(clientIP(c)) {
		fail(c, errRateLimited)
		return
	}
	var req struct {
		IDToken  string `json:"id_token"`
		Nonce    string `json:"nonce"`
		FullName string `json:"full_name"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	id, err := s.google.Verify(ctx, req.IDToken)
	if err != nil {
		fail(c, errInvalidCredentials)
		return
	}
	// The app puts sha256(raw nonce) (hex) into the Google request and sends us the raw nonce.
	if id.Nonce != "" || req.Nonce != "" {
		sum := sha256.Sum256([]byte(req.Nonce))
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(id.Nonce)), []byte(hex.EncodeToString(sum[:]))) != 1 {
			fail(c, errInvalidCredentials)
			return
		}
	}

	var resp *AuthResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var uid uuid.UUID
		err := tx.QueryRow(ctx, `select id from premium_users where google_sub = $1`, id.Sub).Scan(&uid)
		if errors.Is(err, pgx.ErrNoRows) {
			uid, err = createGoogleUser(ctx, tx, id, req.FullName)
		}
		if err != nil {
			return err
		}
		resp, err = s.issueSession(ctx, tx, uid, optionalDeviceID(c))
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func createGoogleUser(ctx context.Context, tx pgx.Tx, id *GoogleIdentity, fullName string) (uuid.UUID, error) {
	local, _, _ := strings.Cut(id.Email, "@")
	base := truncate(loginCharsRe.ReplaceAllString(strings.ToLower(local), ""), 28)
	if len(base) < 3 {
		base = "user" + base
	}
	name := truncate(strings.TrimSpace(fullName), 100)
	if name == "" {
		name = truncate(strings.TrimSpace(id.Name), 100)
	}
	uid := uuid.New()
	for n := 0; n < 1000; n++ {
		login := base
		if n > 0 {
			login = base + strconv.Itoa(n)
		}
		if name == "" {
			name = login
		}
		// Savepoint so a login collision doesn't abort the outer transaction.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return uuid.Nil, err
		}
		_, err = sp.Exec(ctx, `
			insert into premium_users (id, login, full_name, premium_plan_type_index, start_at, expiry_at,
			                           is_active, signup_source, google_sub)
			values ($1, $2, $3, 0, now(), now(), true, 'GOOGLE', $4)`, uid, login, name, id.Sub)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isUniqueViolation(err, "login") {
				continue
			}
			return uuid.Nil, err
		}
		if err = sp.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		// No usable password: "!" never matches a bcrypt hash.
		_, err = tx.Exec(ctx, `insert into auth_credentials (user_id, password_hash) values ($1, '!')`, uid)
		return uid, err
	}
	return uuid.Nil, errors.New("could not allocate login for google user")
}
