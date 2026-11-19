package api

// Integration test harness: every test gets its own freshly migrated Postgres database.
//
//   TEST_DATABASE_URL=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable go test ./...
//
// Tests are skipped when TEST_DATABASE_URL is not set.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Qarznoma/qarznoma-backend/internal/config"
	"github.com/Qarznoma/qarznoma-backend/internal/db"
)

type testEnv struct {
	t          *testing.T
	ts         *httptest.Server
	srv        *Server
	db         *pgxpool.Pool
	adminToken string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "qz_test_" + hex.EncodeToString(b)

	adminConn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminConn.Exec(context.Background(), "drop database if exists "+name+" with (force)")
		_ = adminConn.Close(context.Background())
	})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		JWTSecret:     []byte("test-secret-test-secret-test-secret!"),
		AccessTTL:     15 * time.Minute,
		RefreshTTL:    24 * time.Hour,
		AdminLogin:    "admin",
		AdminPassword: "adminpass1",
	}
	s := New(cfg, pool)
	s.google = &fakeGoogle{}
	// Generous limits so tests aren't throttled (rate limiting has its own test).
	for _, rl := range []**rateLimiter{&s.rlLogin, &s.rlRegister, &s.rlLoginCheck, &s.rlGoogle, &s.rlDeletion, &s.rlSync} {
		*rl = newRateLimiter(100000, time.Minute)
	}
	if err := s.EnsureAdmin(ctx); err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	e := &testEnv{t: t, ts: ts, srv: s, db: pool}
	e.adminToken = e.login("admin", "adminpass1").AccessToken
	return e
}

type resp struct {
	Status int
	Body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

func (r resp) code(t *testing.T) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func (e *testEnv) do(method, path, token string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = bytes.NewBufferString(b)
		default:
			buf, err := json.Marshal(b)
			if err != nil {
				e.t.Fatal(err)
			}
			rd = bytes.NewReader(buf)
		}
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Body: data}
}

// must asserts the status code.
func (e *testEnv) must(status int, method, path, token string, body any) resp {
	e.t.Helper()
	r := e.do(method, path, token, body)
	if r.Status != status {
		e.t.Fatalf("%s %s: want %d, got %d: %s", method, path, status, r.Status, r.Body)
	}
	return r
}

func (e *testEnv) login(login, pw string) AuthResponse {
	e.t.Helper()
	var a AuthResponse
	e.must(200, "POST", "/v1/auth/login", "", map[string]string{"login": login, "password": pw}).json(e.t, &a)
	return a
}

type userOpts struct {
	plan   int16
	expiry time.Time
	active bool
}

func proOpts() userOpts {
	return userOpts{plan: PlanPro, expiry: time.Now().AddDate(1, 0, 0), active: true}
}

// createUser creates a user via the admin API and returns (user, session).
func (e *testEnv) createUser(login string, o userOpts) (PremiumUser, AuthResponse) {
	e.t.Helper()
	var u PremiumUser
	e.must(201, "POST", "/v1/admin/users", e.adminToken, map[string]any{
		"login": login, "password": "secret1", "full_name": login,
		"premium_plan_type_index": o.plan, "expiry_at": o.expiry.UTC().Format(time.RFC3339), "is_active": o.active,
	}).json(e.t, &u)
	return u, e.login(login, "secret1")
}

type pullResp struct {
	Rows    []map[string]any `json:"rows"`
	HasMore bool             `json:"has_more"`
}

func (e *testEnv) pull(token, table string, since int64, afterID string, limit int) pullResp {
	e.t.Helper()
	q := fmt.Sprintf("/v1/sync/%s?since=%d&limit=%d", table, since, limit)
	if afterID != "" {
		q += "&after_id=" + afterID
	}
	var p pullResp
	e.must(200, "GET", q, token, nil).json(e.t, &p)
	return p
}

func (e *testEnv) pullAll(token, table string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	var since int64
	after := ""
	for {
		p := e.pull(token, table, since, after, 1000)
		out = append(out, p.Rows...)
		if !p.HasMore {
			return out
		}
		last := p.Rows[len(p.Rows)-1]
		since = int64(last["last_updated"].(float64))
		after = fmt.Sprint(last["id"])
	}
}

func (e *testEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v", err)
	}
	return n
}

func debtorRow(id string) map[string]any {
	return map[string]any{"id": id, "full_name": "Ali", "repayment_date": "2026-11-30 12:00:00+00", "currency_type": 1}
}

func debtRow(id, debtorID string, amount int64) map[string]any {
	return map[string]any{"id": id, "debtor_id": debtorID, "debt_amount": amount, "debt_date": "2026-11-17 12:42:00+00"}
}

// fakeGoogle accepts tokens of the form "sub|email|nonce".
type fakeGoogle struct{}

func (fakeGoogle) Verify(_ context.Context, tok string) (*GoogleIdentity, error) {
	var parts [3]string
	n := 0
	start := 0
	for i := 0; i <= len(tok) && n < 3; i++ {
		if i == len(tok) || tok[i] == '|' {
			parts[n] = tok[start:i]
			n++
			start = i + 1
		}
	}
	if n != 3 || parts[0] == "" {
		return nil, fmt.Errorf("bad token")
	}
	return &GoogleIdentity{Sub: parts[0], Email: parts[1], Nonce: parts[2]}, nil
}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
