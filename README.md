# Qarznoma — Go backend

Replaces Supabase for the Qarznoma Android app. Contract: `docs/GO_BACKEND_HANDOFF.md` and
`docs/BACKEND_API_SPEC.md` from the handoff bundle. JSON field names and paths follow it exactly.

Stack: Go 1.26, Gin, pgx/v5 (hand-written SQL), goose (embedded migrations, applied on start), slog, JWT HS256.

## Run locally

```bash
cp .env.example .env   # fill in secrets
docker compose up --build
curl localhost:8080/healthz
```

Without Docker: `DATABASE_URL=postgres://... JWT_SECRET=<32+ bytes> go run ./cmd/server`.

| Env | Meaning |
|---|---|
| `DATABASE_URL` | Postgres URL (required) |
| `JWT_SECRET` | ≥ 32 bytes (required) |
| `ADMIN_LOGIN` / `ADMIN_PASSWORD` | bootstrap admin, created on start if the login doesn't exist; never modified afterwards |
| `GOOGLE_WEB_CLIENT_ID` | `aud` for Google ID tokens; empty → `/v1/auth/google` returns 501 |
| `TRUST_PROXY=1` | use `X-Forwarded-For` (behind Caddy/Nginx) for rate limits |
| `WEB_DIR` | serves `web/` at `/privacy` and `/delete-account` (set in the Docker image) |
| `WEB_ORIGIN` | CORS origin if the delete-account page lives on another domain |
| `ACCESS_TTL` / `REFRESH_TTL` | default 15m / 60 days |

## Tests

Integration tests run against a real Postgres. Each test creates its own database, applies the
migrations, and drops it afterwards. They are skipped when `TEST_DATABASE_URL` is unset.

```bash
docker run -d --name qz-pg -e POSTGRES_HOST_AUTH_METHOD=trust -p 5432:5432 postgres:16-alpine
```

```bash
TEST_DATABASE_URL="postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable" go test ./... -count=1
```

`internal/api/acceptance_test.go` covers all 12 acceptance tests in HANDOFF §10, plus refresh grace,
the device-limit race, concurrent sync-clock ordering, plan limits, JSON shape round-trips for every
sync table, self-registration, Google sign-in (fake verifier), announcements, the admin API, the error
envelope, and rate limiting.

## Layout

```
cmd/server         main: config, migrate, bootstrap admin, HTTP server
migrations/        goose SQL (final schema = HANDOFF §4 + sync_clock + purchases)
internal/api       handlers: auth, profile/devices/announcements, sync, admin, deletion, billing
internal/auth      bcrypt, JWT, refresh tokens
web/               privacy-policy.html, delete-account.html (from docs/web)
```

## How the server guarantees (HANDOFF §3) are implemented

| Rule | Where |
|---|---|
| 3.1 ownership | user id only from JWT; `premium_user_id` in body must match or the chunk gets 422; upsert `... where t.premium_user_id = excluded.premium_user_id` (foreign id → 0 rows → 422); debts are checked through their debtor |
| 3.2 PRO-only writes | `writeCheck` before decoding and again under `select ... for update` in the transaction |
| 3.3 `last_updated` | single-row `sync_clock`: `greatest(last_ms+1, now_ms)` locked until commit, so cursors are monotonic **and in commit order** (no in-flight gap) |
| 3.4 tombstone | `is_deleted = t.is_deleted or excluded.is_deleted` |
| 3.5 totals | after a debts push, old and new debtors are recomputed and get the same `last_updated` |
| 3.6 device limit | `for update` on the user row; trimmed when the admin lowers `max_devices` |
| 3.7 creds change | `credentials_updated_at` bump + all refresh tokens revoked; access JWT carries `cv` and is checked on every request, so old access tokens die immediately too |
| 3.8 keyset pull | `(last_updated, id) > (since, after_id)`, `limit+1` to compute `has_more` |
| 3.9 all-or-nothing | full validation first, then one transaction with a pgx batch |
| 7 plan limits | new live debtors/debts counted against `app_settings.pro_max_*` (default 1000/3000) → `409 PLAN_LIMIT` |

## Additions beyond the spec

- **Refresh grace:** a refresh token revoked *by rotation* is accepted again for 60 s, so a lost
  `/refresh` response on a bad network doesn't log the user out. Logout and admin revocation are immediate.
- Unknown user (deleted) with a still-valid access token → `401` (not 404), so the client logs out.
- `GET /v1/me` also returns `signup_source` (extra fields are ignored by the client).
- `POST /v1/admin/payments` only records the payment. It does not extend expiry, because the admin app
  already does that with `PUT /admin/users/{id}` and doing both would double it.

## Not done yet

- Google Play Billing (`/v1/billing/google/verify`, `/rtdn`) is phase 2 and returns `501 NOT_CONFIGURED`.
- No admin notification on new deletion requests (Telegram/email).
- Rate limits are in-memory per instance.
