// Package auth: bcrypt passwords, HS256 access JWTs, opaque refresh tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const BcryptCost = 11

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	return string(h), err
}

// CheckPassword accepts Supabase-imported bcrypt hashes ($2a$/$2b$) too.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash is compared against when the login doesn't exist (constant-ish timing).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), BcryptCost)

func BurnPasswordCheck(pw string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) }

// Claims of an access token.
type Claims struct {
	Role string `json:"role"`
	// CV = credentials_updated_at (unix ms) at issue time. If the admin changes login/password
	// the DB value moves forward and every outstanding access token becomes invalid (3.7).
	CV int64 `json:"cv"`
	jwt.RegisteredClaims
}

type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer { return &Issuer{secret: secret, ttl: ttl} }

func (i *Issuer) TTL() time.Duration { return i.ttl }

func (i *Issuer) Issue(userID uuid.UUID, role string, credsUpdatedAt time.Time) (string, error) {
	now := time.Now()
	c := Claims{
		Role: role,
		CV:   credsUpdatedAt.UnixMilli(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
			Issuer:    "qarznoma",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(i.secret)
}

func (i *Issuer) Parse(tok string) (*Claims, uuid.UUID, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(tok, &c, func(t *jwt.Token) (any, error) {
		return i.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("qarznoma"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, uuid.Nil, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return nil, uuid.Nil, errors.New("bad subject")
	}
	return &c, id, nil
}

// NewRefreshToken returns the opaque token for the client and its sha256 hash for the DB.
func NewRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

func HashRefreshToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}
