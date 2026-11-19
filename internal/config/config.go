// Package config loads server configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr        string // HTTP_ADDR, default ":8080"
	DatabaseURL string // DATABASE_URL (required)
	JWTSecret   []byte // JWT_SECRET (required, >= 32 bytes)

	AccessTTL  time.Duration // ACCESS_TTL, default 15m
	RefreshTTL time.Duration // REFRESH_TTL, default 60 days

	// Bootstrap admin: created on startup if no user with this login exists.
	AdminLogin    string // ADMIN_LOGIN
	AdminPassword string // ADMIN_PASSWORD

	// Google Sign-In: Web client ID used in the Android app (aud of the ID token).
	GoogleWebClientID string // GOOGLE_WEB_CLIENT_ID; empty => /v1/auth/google disabled

	// Allowed origin for the web delete-account page if hosted on another domain (CORS).
	WebOrigin string // WEB_ORIGIN

	// Directory with docs/web pages served at /privacy and /delete-account (optional).
	WebDir string // WEB_DIR

	TrustProxy bool // TRUST_PROXY=1: use X-Forwarded-For for client IP (behind Caddy/Nginx)
}

func Load() (*Config, error) {
	c := &Config{
		Addr:              env("HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		JWTSecret:         []byte(os.Getenv("JWT_SECRET")),
		AdminLogin:        os.Getenv("ADMIN_LOGIN"),
		AdminPassword:     os.Getenv("ADMIN_PASSWORD"),
		GoogleWebClientID: os.Getenv("GOOGLE_WEB_CLIENT_ID"),
		WebOrigin:         os.Getenv("WEB_ORIGIN"),
		WebDir:            os.Getenv("WEB_DIR"),
		TrustProxy:        os.Getenv("TRUST_PROXY") == "1",
	}
	var err error
	if c.AccessTTL, err = duration("ACCESS_TTL", 15*time.Minute); err != nil {
		return nil, err
	}
	if c.RefreshTTL, err = duration("REFRESH_TTL", 60*24*time.Hour); err != nil {
		return nil, err
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT_SECRET must be at least 32 bytes")
	}
	if (c.AdminLogin == "") != (c.AdminPassword == "") {
		return nil, fmt.Errorf("ADMIN_LOGIN and ADMIN_PASSWORD must be set together")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func duration(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("%s: invalid duration %q", k, v)
}
