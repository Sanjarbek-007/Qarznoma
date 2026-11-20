# Qarznoma — Go backend

Replaces Supabase for the Qarznoma Android app. Contract: `docs/GO_BACKEND_HANDOFF.md` and
`docs/BACKEND_API_SPEC.md` from the handoff bundle. JSON field names and paths follow it exactly.

Stack: Go 1.26, Gin, pgx/v5 (hand-written SQL), goose (embedded migrations, applied on start), slog, JWT HS256.

## Run locally

```bash
cp .env.example .env   # fill in secrets, set DOMAIN=localhost
docker compose up --build
curl -k https://localhost/healthz
```

Without Docker: `DATABASE_URL=postgres://... JWT_SECRET=<32+ bytes> go run ./cmd/server`.

## API docs (Swagger)

Swagger UI is at **`/docs`** (e.g. `https://api.example.uz/docs`, or `http://localhost:8080/docs`
locally). Click **Authorize**, paste an `access_token` from `/v1/auth/login`, and every endpoint can
be tried from the browser. The raw spec is at `/docs/openapi.yaml` (source: [docs/openapi.yaml](docs/openapi.yaml)):
import it into Postman, or generate a client from it (e.g. `openapi-generator` for Kotlin/TypeScript).

`docs_test.go` fails if a route is added without documenting it, if a documented route no longer
exists, or if a sync row schema drifts from the server's column list, so update the YAML together
with the handlers. Set `DISABLE_DOCS=1` to turn `/docs` off.

## Deploy to a server

The stack is `db` (Postgres) + `api` + `caddy`. Caddy is the only public part: it listens on
80/443, gets and renews the Let's Encrypt certificate for `DOMAIN`, redirects http → https and
proxies to the API. Postgres and the API are not reachable from the internet.

1. **DNS:** at your domain registrar (or Cloudflare), add an `A` record: name `api` (or whatever
   subdomain you want), value = the server's public IPv4. Add an `AAAA` record too only if the
   server has IPv6. If you use Cloudflare, set the record to "DNS only" (grey cloud) at least until the
   first certificate is issued. Check with `dig +short api.example.uz` that it returns the server IP.
2. **Firewall:** open ports 80 and 443 (TCP, and 443/UDP for HTTP/3). Port 80 is needed for the
   certificate challenge.
3. **Server:** install Docker, copy the repo, then:
   ```bash
   cp .env.example .env   # DOMAIN=api.example.uz, real secrets (openssl rand -hex 32)
   docker compose up -d --build
   docker compose logs -f caddy   # wait for "certificate obtained successfully"
   curl https://api.example.uz/healthz
   ```
4. Point the Android app's base URL at `https://api.example.uz`. The privacy policy and
   delete-account pages are at `https://api.example.uz/privacy` and `/delete-account` (use these
   links in Google Play Console).

Updating: with CI/CD set up (below), merging to `main` deploys automatically. By hand:
`git pull && docker compose up -d --build`. Don't delete the `caddy_data` volume, because
re-issuing certificates too often hits Let's Encrypt rate limits.

| Env | Meaning |
|---|---|
| `DOMAIN` | public domain for Caddy (HTTPS); `localhost` for local testing |
| `DATABASE_URL` | Postgres URL (required) |
| `JWT_SECRET` | ≥ 32 bytes (required) |
| `ADMIN_LOGIN` / `ADMIN_PASSWORD` | bootstrap admin, created on start if the login doesn't exist; never modified afterwards |
| `GOOGLE_WEB_CLIENT_ID` | `aud` for Google ID tokens; empty → `/v1/auth/google` returns 501 |
| `TRUST_PROXY=1` | use `X-Forwarded-For` for rate limits (set automatically in docker-compose, behind Caddy) |
| `WEB_DIR` | serves `web/` at `/privacy` and `/delete-account` (set in the Docker image) |
| `WEB_ORIGIN` | browser origins allowed to call the API (CORS), comma-separated, e.g. `https://app.example.uz,http://localhost:5173` |
| `API_IMAGE` | image for the `api` service; set by the deploy script on the server, leave unset locally |
| `ACCESS_TTL` / `REFRESH_TTL` | default 15m / 60 days |
| `DISABLE_DOCS=1` | turn off Swagger UI at `/docs` |

## CI/CD (GitHub Actions)

**CI** ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs on every pull request:
gofmt, `go mod tidy` check, `go vet`, `govulncheck` (known vulnerabilities in dependencies and the Go
standard library), the full test suite with `-race` against Postgres, and a Docker build.

**CD** ([.github/workflows/deploy.yml](.github/workflows/deploy.yml)) runs on every push to `main`:

1. runs CI again;
2. builds the image and pushes it to GHCR as `ghcr.io/<owner>/qarznoma-api:<commit sha>` (and `:latest`);
3. if deploying is enabled: copies `docker-compose.yml`, `Caddyfile` and
   [scripts/deploy.sh](scripts/deploy.sh) to the server over SSH and runs the script, which pulls
   the image, restarts the API and waits for `/healthz`. If the new version isn't healthy within
   about 60 s, or the deploy is interrupted, it switches back to the previous version and the run fails.

Until deploying is enabled, merges to `main` only run CI and push the image.

### One-time setup

**On the server** (after the steps in "Deploy to a server" above, minus `docker compose up`):

```bash
sudo adduser --disabled-password --gecos "" deploy && sudo usermod -aG docker deploy
sudo mkdir -p /opt/qarznoma && sudo chown deploy: /opt/qarznoma
sudo -u deploy cp .env.example /opt/qarznoma/.env   # then edit it: DOMAIN and real secrets
```

**A deploy key** (on your laptop; this key only lets GitHub log in as `deploy`):

```bash
ssh-keygen -t ed25519 -N "" -C github-deploy -f qarznoma_deploy
```

Append `qarznoma_deploy.pub` to `/home/deploy/.ssh/authorized_keys` on the server. Then get the
server's host key, and check its fingerprint against the server (`ssh-keygen -lf` on
`/etc/ssh/ssh_host_ed25519_key.pub`):

```bash
ssh-keyscan -t ed25519 <server-ip>
```

**On GitHub**, Settings → Environments → New environment `production` (optionally add yourself as
a required reviewer, so every deploy waits for your click). In that environment add:

| Secret | Value |
|---|---|
| `SSH_HOST` | server IP or hostname |
| `SSH_USER` | `deploy` |
| `SSH_PRIVATE_KEY` | contents of `qarznoma_deploy` (the private key; delete it from your laptop afterwards) |
| `SSH_KNOWN_HOSTS` | the `ssh-keyscan` output line |

Then Settings → Secrets and variables → Actions → Variables:

| Variable | Value |
|---|---|
| `DEPLOY_ENABLED` | `true` (turns deploying on) |
| `DEPLOY_PATH` | optional, default `/opt/qarznoma` |
| `SSH_PORT` | optional, default `22` |
| `PUBLIC_URL` | optional, e.g. `https://api.example.uz` (shown as a link on the deploy run) |

The image is private on GHCR; the workflow's own token pulls it during the deploy, so the server
needs no GitHub credentials. Also recommended: Settings → Branches → protect `main` and require the
CI checks (Lint, Test, Docker build) to pass before merging.

### Rolling back

Actions → Deploy → Run workflow → enter an older commit SHA as `tag`. It deploys that image without
rebuilding. Migrations only run forward, so roll back only to a version made for the same database
schema (any version after the last migration was added).

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
docs/              openapi.yaml (embedded, served at /docs)
scripts/deploy.sh  runs on the server during CD: pull, restart, health check, rollback
.github/workflows  ci.yml (checks on every PR), deploy.yml (build, push to GHCR, deploy on main)
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
