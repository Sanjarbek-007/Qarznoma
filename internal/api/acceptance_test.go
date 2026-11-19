package api

// Acceptance tests — docs/GO_BACKEND_HANDOFF.md §10 (numbered as there), plus the other
// MANDATORY server guarantees from §3, §5 and §7.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// §10.1 login ok/bad; refresh rotation; refresh after logout → 401.
func TestAuthLoginRefreshLogout(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("10234", proOpts())
	if s.UserID != u.ID || s.ExpiresIn != 900 || s.AccessToken == "" || s.RefreshToken == "" {
		t.Fatalf("bad auth response: %+v", s)
	}
	r := e.do("POST", "/v1/auth/login", "", map[string]string{"login": "10234", "password": "wrong"})
	if r.Status != 401 || r.code(t) != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %d %s", r.Status, r.Body)
	}
	r = e.do("POST", "/v1/auth/login", "", map[string]string{"login": "nobody", "password": "x"})
	if r.Status != 401 || r.code(t) != "INVALID_CREDENTIALS" {
		t.Fatalf("unknown login: %d %s", r.Status, r.Body)
	}

	var s2 AuthResponse
	e.must(200, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken}).json(t, &s2)
	if s2.RefreshToken == s.RefreshToken || s2.UserID != u.ID {
		t.Fatalf("refresh not rotated: %+v", s2)
	}
	e.must(200, "GET", "/v1/me", s2.AccessToken, nil)

	e.must(204, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": s2.RefreshToken})
	r = e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s2.RefreshToken})
	if r.Status != 401 || r.code(t) != "UNAUTHORIZED" {
		t.Fatalf("refresh after logout: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": "garbage"}); r.Status != 401 {
		t.Fatalf("garbage refresh: %d", r.Status)
	}
}

// Rotated token is accepted once more within the grace window; after that it's dead.
func TestRefreshGraceWindow(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("grace", proOpts())
	e.must(200, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken})
	// Response "lost": client retries with the old token.
	e.must(200, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken})
	// Push the rotation outside the grace window.
	_, err := e.db.Exec(context.Background(), `update refresh_tokens set revoked_at = now() - interval '2 minutes' where revoked_at is not null`)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken}); r.Status != 401 {
		t.Fatalf("expired grace: %d", r.Status)
	}
}

// §10.2 admin changes password → old refresh 401, old access 401, credentials_updated_at changed.
// Non-credential edits don't revoke.
func TestCredentialsChangeRevokes(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("10235", proOpts())

	var after PremiumUser
	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken,
		map[string]any{"full_name": "Yangi ism", "premium_plan_type_index": 2}).json(t, &after)
	if !after.CredentialsUpdatedAt.Equal(u.CredentialsUpdatedAt.Time) {
		t.Fatal("non-credential edit changed credentials_updated_at")
	}
	e.must(200, "GET", "/v1/me", s.AccessToken, nil)

	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken, map[string]any{"password": "newpass1"}).json(t, &after)
	if !after.CredentialsUpdatedAt.After(u.CredentialsUpdatedAt.Time) {
		t.Fatal("credentials_updated_at not bumped")
	}
	if r := e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken}); r.Status != 401 {
		t.Fatalf("old refresh: %d", r.Status)
	}
	if r := e.do("GET", "/v1/me", s.AccessToken, nil); r.Status != 401 {
		t.Fatalf("old access: %d", r.Status)
	}
	e.login("10235", "newpass1")

	// Login change revokes too.
	s3 := e.login("10235", "newpass1")
	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken, map[string]any{"login": "renamed"})
	if r := e.do("GET", "/v1/me", s3.AccessToken, nil); r.Status != 401 {
		t.Fatalf("access after login change: %d", r.Status)
	}
	e.login("renamed", "newpass1")
}

// §10.3 device limit 1: first PUT 200, second new device 409, first again 200.
func TestDeviceLimit(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("dev", proOpts())
	e.must(200, "PUT", "/v1/devices/d1", s.AccessToken, map[string]string{"device_name": "Xiaomi"})
	r := e.do("PUT", "/v1/devices/d2", s.AccessToken, map[string]string{"device_name": "Other"})
	if r.Status != 409 || r.code(t) != "DEVICE_LIMIT_REACHED" {
		t.Fatalf("second device: %d %s", r.Status, r.Body)
	}
	e.must(200, "PUT", "/v1/devices/d1", s.AccessToken, map[string]string{"device_name": "Xiaomi 2"})
	e.must(200, "GET", "/v1/devices/d1", s.AccessToken, nil)
	e.must(404, "GET", "/v1/devices/d2", s.AccessToken, nil)

	// Raise limit, add two more, then lower → oldest trimmed.
	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken, map[string]any{"max_devices": 3})
	e.must(200, "PUT", "/v1/devices/d2", s.AccessToken, map[string]string{})
	e.must(200, "PUT", "/v1/devices/d3", s.AccessToken, map[string]string{})
	_, _ = e.db.Exec(context.Background(), `update user_devices set last_seen_at = now() - interval '1 day' where device_id = 'd1'`)
	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken, map[string]any{"max_devices": 2})
	e.must(404, "GET", "/v1/devices/d1", s.AccessToken, nil)
	e.must(200, "GET", "/v1/devices/d2", s.AccessToken, nil)

	e.must(204, "DELETE", "/v1/devices/d2", s.AccessToken, nil)
	var ds []Device
	e.must(200, "GET", "/v1/devices", s.AccessToken, nil).json(t, &ds)
	if len(ds) != 1 || ds[0].DeviceID != "d3" {
		t.Fatalf("devices: %+v", ds)
	}
}

// Concurrent registration of new devices with limit 1 → exactly one wins.
func TestDeviceLimitRace(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("race", proOpts())
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.do("PUT", fmt.Sprintf("/v1/devices/r%d", i), s.AccessToken, map[string]string{}).Status
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		} else if c != 409 {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("want exactly 1 device, got %d (%v)", ok, codes)
	}
}

// §10.4 FREE push 403; PRO 204; expired PRO 403; blocked 403; PULL always 200.
func TestPushRequiresActivePro(t *testing.T) {
	e := newTestEnv(t)
	body := []any{debtorRow(uuid.NewString())}
	cases := []struct {
		name string
		o    userOpts
		code int
		err  string
	}{
		{"free", userOpts{plan: PlanFree, expiry: time.Now().AddDate(1, 0, 0), active: true}, 403, "FORBIDDEN_NOT_PRO"},
		{"plus", userOpts{plan: PlanPlus, expiry: time.Now().AddDate(1, 0, 0), active: true}, 403, "FORBIDDEN_NOT_PRO"},
		{"pro_expired", userOpts{plan: PlanPro, expiry: time.Now().Add(-time.Hour), active: true}, 403, "FORBIDDEN_NOT_PRO"},
		{"pro_blocked", userOpts{plan: PlanPro, expiry: time.Now().AddDate(1, 0, 0), active: false}, 403, "ACCOUNT_BLOCKED"},
		{"pro", proOpts(), 204, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, s := e.createUser(c.name, c.o)
			for _, table := range []string{"debtors", "finance_tags", "notes"} {
				b := any(body)
				if table == "finance_tags" {
					b = []any{map[string]any{"id": uuid.NewString(), "name": "Ish", "color_key": "C1"}}
				} else if table == "notes" {
					b = []any{map[string]any{"id": uuid.NewString(), "title": "x"}}
				}
				r := e.do("POST", "/v1/sync/"+table, s.AccessToken, b)
				if r.Status != c.code || (c.err != "" && r.code(t) != c.err) {
					t.Fatalf("%s push: %d %s", table, r.Status, r.Body)
				}
				e.must(200, "GET", "/v1/sync/"+table+"?since=0", s.AccessToken, nil)
			}
			// Blocked users can still read /me (is_active=false).
			e.must(200, "GET", "/v1/me", s.AccessToken, nil)
		})
	}
	// Expired PRO can still pull what it wrote before expiry.
	u, s := e.createUser("was_pro", proOpts())
	id := uuid.NewString()
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(id)})
	e.must(200, "PUT", "/v1/admin/users/"+u.ID.String(), e.adminToken,
		map[string]any{"expiry_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
	if rows := e.pullAll(s.AccessToken, "debtors"); len(rows) != 1 || rows[0]["id"] != id {
		t.Fatalf("expired pull: %v", rows)
	}
}

// §10.5 foreign premium_user_id / foreign row id → whole chunk rejected, nothing written.
func TestOwnership(t *testing.T) {
	e := newTestEnv(t)
	a, sa := e.createUser("owner_a", proOpts())
	_, sb := e.createUser("owner_b", proOpts())

	dA := uuid.NewString()
	e.must(204, "POST", "/v1/sync/debtors", sa.AccessToken, []any{debtorRow(dA)})

	// B claims A's premium_user_id.
	row := debtorRow(uuid.NewString())
	row["premium_user_id"] = a.ID.String()
	r := e.do("POST", "/v1/sync/debtors", sb.AccessToken, []any{debtorRow(uuid.NewString()), row})
	if r.Status != 422 {
		t.Fatalf("foreign premium_user_id: %d %s", r.Status, r.Body)
	}
	// B overwrites A's row id.
	hijack := debtorRow(dA)
	hijack["full_name"] = "HACKED"
	newID := uuid.NewString()
	r = e.do("POST", "/v1/sync/debtors", sb.AccessToken, []any{debtorRow(newID), hijack})
	if r.Status != 422 {
		t.Fatalf("hijack: %d %s", r.Status, r.Body)
	}
	if n := e.count(`select count(*) from debtors where id = $1`, newID); n != 0 {
		t.Fatal("partial chunk was written")
	}
	if n := e.count(`select count(*) from debtors where id = $1 and full_name = 'Ali'`, dA); n != 1 {
		t.Fatal("A's debtor was modified")
	}
	// B adds a debt to A's debtor; B moves A's debt.
	debtA := uuid.NewString()
	e.must(204, "POST", "/v1/sync/debts", sa.AccessToken, []any{debtRow(debtA, dA, -100)})
	if r := e.do("POST", "/v1/sync/debts", sb.AccessToken, []any{debtRow(uuid.NewString(), dA, -5)}); r.Status != 422 {
		t.Fatalf("debt on foreign debtor: %d", r.Status)
	}
	dB := uuid.NewString()
	e.must(204, "POST", "/v1/sync/debtors", sb.AccessToken, []any{debtorRow(dB)})
	if r := e.do("POST", "/v1/sync/debts", sb.AccessToken, []any{debtRow(debtA, dB, -5)}); r.Status != 422 {
		t.Fatalf("steal debt: %d", r.Status)
	}
	// B sees nothing of A.
	if rows := e.pullAll(sb.AccessToken, "debts"); len(rows) != 0 {
		t.Fatalf("B sees A's debts: %v", rows)
	}
	var byIDs []map[string]any
	e.must(200, "POST", "/v1/sync/debtors/by-ids", sb.AccessToken, map[string]any{"ids": []string{dA, dB}}).json(t, &byIDs)
	if len(byIDs) != 1 || byIDs[0]["id"] != dB {
		t.Fatalf("by-ids leaks: %v", byIDs)
	}
}

func debtorTotal(t *testing.T, e *testEnv, id string) int64 {
	t.Helper()
	var v int64
	if err := e.db.QueryRow(context.Background(), `select total_debts_amount from debtors where id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// §10.6 totals on add/edit/delete; moving a debt recomputes both debtors (and bumps last_updated).
func TestDebtorTotals(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("totals", proOpts())
	d1, d2 := uuid.NewString(), uuid.NewString()
	r1 := debtorRow(d1)
	r1["total_debts_amount"] = 999999 // ignored
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{r1, debtorRow(d2)})
	if debtorTotal(t, e, d1) != 0 {
		t.Fatal("client total_debts_amount not ignored")
	}
	x, y := uuid.NewString(), uuid.NewString()
	e.must(204, "POST", "/v1/sync/debts", s.AccessToken, []any{debtRow(x, d1, -123450), debtRow(y, d1, 23450)})
	if v := debtorTotal(t, e, d1); v != -100000 {
		t.Fatalf("after insert: %d", v)
	}
	e.must(204, "POST", "/v1/sync/debts", s.AccessToken, []any{debtRow(y, d1, 50000)})
	if v := debtorTotal(t, e, d1); v != -73450 {
		t.Fatalf("after edit: %d", v)
	}
	before := e.pullAll(s.AccessToken, "debtors")
	var lu2Before float64
	for _, r := range before {
		if r["id"] == d2 {
			lu2Before = r["last_updated"].(float64)
		}
	}
	e.must(204, "POST", "/v1/sync/debts", s.AccessToken, []any{debtRow(y, d2, 50000)})
	if a, b := debtorTotal(t, e, d1), debtorTotal(t, e, d2); a != -123450 || b != 50000 {
		t.Fatalf("after move: %d %d", a, b)
	}
	for _, r := range e.pullAll(s.AccessToken, "debtors") {
		if r["id"] == d2 && r["last_updated"].(float64) <= lu2Before {
			t.Fatal("recompute did not bump debtor last_updated")
		}
	}
	del := debtRow(x, d1, -123450)
	del["is_deleted"] = true
	e.must(204, "POST", "/v1/sync/debts", s.AccessToken, []any{del})
	if v := debtorTotal(t, e, d1); v != 0 {
		t.Fatalf("after delete: %d", v)
	}
}

// §10.7 tombstone is final; other fields may still change.
func TestTombstoneFinal(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("tomb", proOpts())
	id := uuid.NewString()
	row := debtorRow(id)
	row["is_deleted"] = true
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{row})
	row["is_deleted"] = false
	row["full_name"] = "Edited on old device"
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{row})
	rows := e.pullAll(s.AccessToken, "debtors")
	if len(rows) != 1 || rows[0]["is_deleted"] != true || rows[0]["full_name"] != "Edited on old device" {
		t.Fatalf("tombstone: %v", rows)
	}
	// Same for a finance table.
	tag := map[string]any{"id": uuid.NewString(), "name": "x", "color_key": "C1", "is_deleted": true}
	e.must(204, "POST", "/v1/sync/finance_tags", s.AccessToken, []any{tag})
	tag["is_deleted"] = false
	e.must(204, "POST", "/v1/sync/finance_tags", s.AccessToken, []any{tag})
	if rows := e.pullAll(s.AccessToken, "finance_tags"); rows[0]["is_deleted"] != true {
		t.Fatal("finance tombstone revived")
	}
}

// §10.8 2500 rows with the same last_updated → 3 pages, no duplicates, nothing skipped.
func TestPullKeysetPaging(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("paging", proOpts())
	// PRO default is 1000 debtors (§7); lift it for this test.
	if _, err := e.db.Exec(context.Background(), `update app_settings set pro_max_debtors = 10000`); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for b := 0; b < 5; b++ {
		var chunk []any
		for i := 0; i < 500; i++ {
			id := uuid.NewString()
			ids[id] = true
			chunk = append(chunk, debtorRow(id))
		}
		e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, chunk)
	}
	if _, err := e.db.Exec(context.Background(), `update debtors set last_updated = 42 where premium_user_id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	pages := 0
	var since int64
	after := ""
	for {
		p := e.pull(s.AccessToken, "debtors", since, after, 1000)
		pages++
		for _, r := range p.Rows {
			id := r["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
		}
		if !p.HasMore {
			break
		}
		last := p.Rows[len(p.Rows)-1]
		since, after = int64(last["last_updated"].(float64)), last["id"].(string)
	}
	if pages != 3 || len(seen) != 2500 {
		t.Fatalf("pages=%d rows=%d", pages, len(seen))
	}
	for id := range ids {
		if !seen[id] {
			t.Fatalf("skipped %s", id)
		}
	}
	// since past everything → empty.
	if p := e.pull(s.AccessToken, "debtors", 43, "", 1000); len(p.Rows) != 0 || p.HasMore {
		t.Fatal("expected empty page")
	}
}

// §10.9 last_updated is server-assigned and strictly increases across pushes.
func TestLastUpdatedServerAssigned(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("clock", proOpts())
	id := uuid.NewString()
	row := debtorRow(id)
	row["last_updated"] = int64(5)
	start := time.Now().UnixMilli()
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{row})
	lu1 := int64(e.pullAll(s.AccessToken, "debtors")[0]["last_updated"].(float64))
	if lu1 < start-1000 {
		t.Fatalf("client last_updated not ignored: %d", lu1)
	}
	row["last_updated"] = int64(9999999999999)
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{row})
	lu2 := int64(e.pullAll(s.AccessToken, "debtors")[0]["last_updated"].(float64))
	if lu2 <= lu1 || lu2 > time.Now().UnixMilli()+1000 {
		t.Fatalf("lu1=%d lu2=%d", lu1, lu2)
	}
}

// Concurrent pushes from different users get distinct, commit-ordered cursors.
func TestSyncClockConcurrent(t *testing.T) {
	e := newTestEnv(t)
	var sessions []AuthResponse
	for i := 0; i < 4; i++ {
		_, s := e.createUser(fmt.Sprintf("conc%d", i), proOpts())
		sessions = append(sessions, s)
	}
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if r := e.do("POST", "/v1/sync/debtors", tok, []any{debtorRow(uuid.NewString())}); r.Status != 204 {
					t.Errorf("push: %d %s", r.Status, r.Body)
				}
			}
		}(s.AccessToken)
	}
	wg.Wait()
	if n := e.count(`select count(distinct last_updated) from debtors`); n != 40 {
		t.Fatalf("want 40 distinct cursors, got %d", n)
	}
}

// §10.10 finance_categories: "transport" is independent per user.
func TestFinanceCategoriesPerUser(t *testing.T) {
	e := newTestEnv(t)
	_, sa := e.createUser("cat_a", proOpts())
	_, sb := e.createUser("cat_b", proOpts())
	cat := func(label string, hidden bool) []any {
		return []any{map[string]any{"id": "transport", "label": label, "type": "EXPENSE",
			"color_key": "C2", "icon_key": "CAR", "is_hidden": hidden}}
	}
	e.must(204, "POST", "/v1/sync/finance_categories", sa.AccessToken, cat("", true))
	e.must(204, "POST", "/v1/sync/finance_categories", sb.AccessToken, cat("Taksi", false))
	a := e.pullAll(sa.AccessToken, "finance_categories")
	b := e.pullAll(sb.AccessToken, "finance_categories")
	if len(a) != 1 || a[0]["label"] != "" || a[0]["is_hidden"] != true {
		t.Fatalf("a: %v", a)
	}
	if len(b) != 1 || b[0]["label"] != "Taksi" || b[0]["is_hidden"] != false {
		t.Fatalf("b: %v", b)
	}
	// keyset with text id
	p := e.pull(sa.AccessToken, "finance_categories", 0, "oziq", 10)
	if len(p.Rows) != 1 {
		t.Fatalf("text after_id: %v", p.Rows)
	}
}

// §10.11 500-row chunk with one invalid row → 422, 0 rows written; >500 → 413.
func TestChunkAllOrNothing(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("chunk", proOpts())
	var chunk []any
	for i := 0; i < 500; i++ {
		chunk = append(chunk, map[string]any{"id": uuid.NewString(), "type": "EXPENSE", "currency": "UZS",
			"amount": 100, "tx_date": "2026-11-17 12:42:00+00"})
	}
	chunk[321].(map[string]any)["type"] = "BOGUS"
	r := e.do("POST", "/v1/sync/finance_transactions", s.AccessToken, chunk)
	if r.Status != 422 || r.code(t) != "VALIDATION" {
		t.Fatalf("bad chunk: %d %s", r.Status, r.Body)
	}
	if n := e.count(`select count(*) from finance_transactions`); n != 0 {
		t.Fatalf("%d rows written", n)
	}
	chunk[321].(map[string]any)["type"] = "EXPENSE"
	e.must(204, "POST", "/v1/sync/finance_transactions", s.AccessToken, chunk)
	// Idempotent re-push.
	e.must(204, "POST", "/v1/sync/finance_transactions", s.AccessToken, chunk)
	if n := e.count(`select count(*) from finance_transactions`); n != 500 {
		t.Fatalf("want 500, got %d", n)
	}
	chunk = append(chunk, chunk[0])
	if r := e.do("POST", "/v1/sync/finance_transactions", s.AccessToken, chunk); r.Status != 413 {
		t.Fatalf("501 rows: %d", r.Status)
	}
	// Debts pointing at a non-existent debtor → 422.
	if r := e.do("POST", "/v1/sync/debts", s.AccessToken, []any{debtRow(uuid.NewString(), uuid.NewString(), 1)}); r.Status != 422 {
		t.Fatalf("orphan debt: %d", r.Status)
	}
	// Unknown table → 404.
	if r := e.do("GET", "/v1/sync/premium_users", s.AccessToken, nil); r.Status != 404 {
		t.Fatalf("unknown table: %d", r.Status)
	}
}

// Every sync table round-trips the JSON shapes from §6.
func TestSyncShapesRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("shapes", proOpts())
	debtor := uuid.NewString()
	rows := map[string]map[string]any{
		"debtors": {"id": debtor, "full_name": "Ali", "description": "d", "phone_number": "901234567",
			"repayment_date": "2026-11-30 12:00:00+00", "currency_type": 1, "is_deleted": false, "updated_at_client": 1763380000000},
		"debts": {"id": uuid.NewString(), "debtor_id": debtor, "debt_amount": -123450, "debt_description": "",
			"debt_date": "2026-11-17T12:42:00Z", "is_deleted": false},
		"finance_tags": {"id": uuid.NewString(), "name": "Ish", "color_key": "C1", "is_deleted": false, "updated_at_client": 7},
		"finance_goals": {"id": uuid.NewString(), "name": "Telefon", "note": "", "currency": "UZS", "target_amount": 300000000,
			"saved_amount": 75000000, "color_key": "C1", "icon_key": "SAVINGS", "deadline": nil, "is_deleted": false, "updated_at_client": 0},
		"finance_transactions": {"id": uuid.NewString(), "type": "CONVERSION", "currency": "USD", "amount": 10000,
			"to_currency": "UZS", "to_amount": 128000000, "rate": "12800.00", "category_id": "", "description": "",
			"tag_ids": "a,b", "tx_date": "2026-11-17 12:42:00+00", "linked_debt_id": nil, "is_opening_balance": false,
			"is_deleted": false, "updated_at_client": 0},
		"notes": {"id": uuid.NewString(), "title": "Bozor", "content": `{"blocks":[]}`, "color_key": "DEFAULT",
			"labels": "uy,ish", "is_pinned": true, "is_archived": false, "reminder_at": 1763400000000, "is_deleted": false,
			"updated_at_client": 1763380000000},
	}
	for _, table := range []string{"debtors", "debts", "finance_tags", "finance_goals", "finance_transactions", "notes"} {
		e.must(204, "POST", "/v1/sync/"+table, s.AccessToken, []any{rows[table]})
		got := e.pullAll(s.AccessToken, table)
		if len(got) != 1 {
			t.Fatalf("%s: %d rows", table, len(got))
		}
		for k, want := range rows[table] {
			g := got[0][k]
			switch k {
			case "repayment_date", "debt_date":
				if g != "2026-11-30T12:00:00Z" && g != "2026-11-17T12:42:00Z" {
					t.Fatalf("%s.%s = %v", table, k, g)
				}
				continue
			}
			if fmt.Sprint(g) != fmt.Sprint(normalizeNum(want)) {
				t.Fatalf("%s.%s: want %v, got %v", table, k, want, g)
			}
		}
		if _, ok := got[0]["last_updated"]; !ok {
			t.Fatalf("%s: no last_updated", table)
		}
		if table != "debts" && got[0]["premium_user_id"] != u.ID.String() {
			t.Fatalf("%s: premium_user_id %v", table, got[0]["premium_user_id"])
		}
	}
}

func normalizeNum(v any) any {
	if n, ok := v.(int); ok {
		return float64(n)
	}
	return v
}

// §7 plan limit on PUSH: new live rows over the limit → 409 PLAN_LIMIT; updates/tombstones pass.
func TestPlanLimit(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("limits", proOpts())
	var settings AppSettings
	e.must(200, "GET", "/v1/admin/app-settings", e.adminToken, nil).json(t, &settings)
	two := int32(2)
	settings.ProMaxDebtors = &two
	e.must(200, "PUT", "/v1/admin/app-settings", e.adminToken, settings)

	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(a), debtorRow(b)})
	r := e.do("POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(c)})
	if r.Status != 409 || r.code(t) != "PLAN_LIMIT" {
		t.Fatalf("over limit: %d %s", r.Status, r.Body)
	}
	// Update existing + new tombstone is fine.
	tomb := debtorRow(c)
	tomb["is_deleted"] = true
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(a), tomb})
	// Deleting one frees a slot.
	del := debtorRow(a)
	del["is_deleted"] = true
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{del})
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(uuid.NewString())})
}

// §10.12 web deletion request: 202 and user NOT deleted; repeat 409; bad password 401;
// approve deletes everything + audit; reject leaves the user.
func TestAccountDeletionRequests(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("victim", proOpts())
	d := uuid.NewString()
	e.must(204, "POST", "/v1/sync/debtors", s.AccessToken, []any{debtorRow(d)})
	e.must(204, "POST", "/v1/sync/debts", s.AccessToken, []any{debtRow(uuid.NewString(), d, -1)})
	e.must(200, "PUT", "/v1/devices/d1", s.AccessToken, map[string]string{})

	form := map[string]string{"login": "victim", "password": "secret1", "phone": "+998 90 123 45 67", "lang": "ru"}
	e.must(202, "POST", "/v1/account-deletion-requests", "", form)
	if e.count(`select count(*) from premium_users where id = $1`, u.ID) != 1 {
		t.Fatal("user deleted by the web form")
	}
	if r := e.do("POST", "/v1/account-deletion-requests", "", form); r.Status != 409 || r.code(t) != "REQUEST_EXISTS" {
		t.Fatalf("repeat: %d %s", r.Status, r.Body)
	}
	bad := map[string]string{"login": "victim", "password": "nope", "phone": "+998901234567"}
	if r := e.do("POST", "/v1/account-deletion-requests", "", bad); r.Status != 401 {
		t.Fatalf("bad password: %d", r.Status)
	}
	short := map[string]string{"login": "victim", "password": "secret1", "phone": "12-34"}
	if r := e.do("POST", "/v1/account-deletion-requests", "", short); r.Status != 400 {
		t.Fatalf("short phone: %d", r.Status)
	}

	var reqs []DeletionRequest
	e.must(200, "GET", "/v1/admin/deletion-requests?status=PENDING", e.adminToken, nil).json(t, &reqs)
	if len(reqs) != 1 || reqs[0].Lang != "ru" || reqs[0].Status != "PENDING" || reqs[0].ResolvedAt.Valid {
		t.Fatalf("list: %+v", reqs)
	}
	var approved DeletionRequest
	e.must(200, "POST", "/v1/admin/deletion-requests/"+reqs[0].ID.String()+"/approve", e.adminToken,
		map[string]string{"note": "tel qilindi"}).json(t, &approved)
	if approved.Status != "APPROVED" || !approved.ResolvedAt.Valid || approved.AdminNote != "tel qilindi" {
		t.Fatalf("approved: %+v", approved)
	}
	for _, q := range []string{
		`select count(*) from premium_users where id = $1`,
		`select count(*) from debtors where premium_user_id = $1`,
		`select count(*) from user_devices where premium_user_id = $1`,
		`select count(*) from refresh_tokens where user_id = $1`,
	} {
		if n := e.count(q, u.ID); n != 0 {
			t.Fatalf("%s → %d", q, n)
		}
	}
	if e.count(`select count(*) from debts`) != 0 {
		t.Fatal("debts left")
	}
	if e.count(`select count(*) from admin_audit_log where action = 'USER_DELETE' and target_user_id = $1`, u.ID) != 1 {
		t.Fatal("no audit")
	}
	if r := e.do("POST", "/v1/admin/deletion-requests/"+reqs[0].ID.String()+"/approve", e.adminToken, nil); r.Status != 409 {
		t.Fatalf("re-approve: %d", r.Status)
	}

	// Reject path.
	u2, _ := e.createUser("keeper", proOpts())
	e.must(202, "POST", "/v1/account-deletion-requests", "", map[string]string{"login": "keeper", "password": "secret1", "phone": "+998901234567"})
	e.must(200, "GET", "/v1/admin/deletion-requests?status=PENDING", e.adminToken, nil).json(t, &reqs)
	e.must(200, "POST", "/v1/admin/deletion-requests/"+reqs[0].ID.String()+"/reject", e.adminToken, map[string]string{"note": "javob yo'q"})
	if e.count(`select count(*) from premium_users where id = $1`, u2.ID) != 1 {
		t.Fatal("reject deleted the user")
	}
	if e.count(`select count(*) from admin_audit_log where action = 'DELETE_REQUEST_REJECT'`) != 1 {
		t.Fatal("no reject audit")
	}
	// After rejection a new request is allowed again.
	e.must(202, "POST", "/v1/account-deletion-requests", "", map[string]string{"login": "keeper", "password": "secret1", "phone": "+998901234567"})
}

// §5.1 register, login-available, suggest-login, DELETE /me.
func TestSelfServiceAccounts(t *testing.T) {
	e := newTestEnv(t)
	var a AuthResponse
	e.must(201, "POST", "/v1/auth/register", "", map[string]string{"login": "Ali_2024", "password": "secret1", "full_name": "Ali"}).json(t, &a)
	var me PremiumUser
	e.must(200, "GET", "/v1/me", a.AccessToken, nil).json(t, &me)
	if me.Login != "ali_2024" || me.PremiumPlanTypeIndex != 0 || !me.IsActive || me.SignupSource != "SELF" || me.ExpiryAt.After(time.Now()) {
		t.Fatalf("registered user: %+v", me)
	}
	for _, c := range []struct {
		body       map[string]string
		code, errc string
	}{
		{map[string]string{"login": "ali_2024", "password": "secret1"}, "409", "LOGIN_TAKEN"},
		{map[string]string{"login": "a!", "password": "secret1"}, "400", "INVALID_LOGIN"},
		{map[string]string{"login": "okname", "password": "12345"}, "400", "WEAK_PASSWORD"},
	} {
		r := e.do("POST", "/v1/auth/register", "", c.body)
		if fmt.Sprint(r.Status) != c.code || r.code(t) != c.errc {
			t.Fatalf("%v: %d %s", c.body, r.Status, r.Body)
		}
	}
	avail := func(l string) bool {
		var v struct{ Available bool }
		e.must(200, "GET", "/v1/auth/login-available?login="+l, "", nil).json(t, &v)
		return v.Available
	}
	if avail("ali_2024") || avail("ALI_2024") || avail("ab") || !avail("free_name") {
		t.Fatal("login-available wrong")
	}
	var sug struct{ Login string }
	e.must(200, "GET", "/v1/auth/suggest-login?base=Jasur%20Bek!", "", nil).json(t, &sug)
	if !loginRe.MatchString(sug.Login) || len(sug.Login) != len("jasurbek_1234") || sug.Login[:9] != "jasurbek_" {
		t.Fatalf("suggest: %q", sug.Login)
	}
	e.must(200, "GET", "/v1/auth/suggest-login", "", nil).json(t, &sug)
	if len(sug.Login) != 9 || !avail(sug.Login) {
		t.Fatalf("numeric suggest: %q", sug.Login)
	}

	e.must(204, "DELETE", "/v1/me", a.AccessToken, nil)
	if r := e.do("GET", "/v1/me", a.AccessToken, nil); r.Status != 401 {
		t.Fatalf("after delete: %d", r.Status)
	}
	if !avail("ali_2024") {
		t.Fatal("login not freed")
	}
}

// §5.1 Google sign-in: nonce check, create-or-find by sub, login from email with suffix on clash.
func TestGoogleSignIn(t *testing.T) {
	e := newTestEnv(t)
	e.createUser("jasur", proOpts())
	nonce := "raw-nonce-123"
	sum := sha256.Sum256([]byte(nonce))
	hashed := hex.EncodeToString(sum[:])

	var a AuthResponse
	e.must(200, "POST", "/v1/auth/google", "", map[string]string{
		"id_token": "sub-1|Jasur@gmail.com|" + hashed, "nonce": nonce, "full_name": "Jasur B"}).json(t, &a)
	var me PremiumUser
	e.must(200, "GET", "/v1/me", a.AccessToken, nil).json(t, &me)
	if me.Login != "jasur1" || me.SignupSource != "GOOGLE" || me.FullName != "Jasur B" || me.PremiumPlanTypeIndex != 0 {
		t.Fatalf("google user: %+v", me)
	}
	var again AuthResponse
	e.must(200, "POST", "/v1/auth/google", "", map[string]string{
		"id_token": "sub-1|Jasur@gmail.com|" + hashed, "nonce": nonce}).json(t, &again)
	if again.UserID != a.UserID {
		t.Fatal("second sign-in created a new account")
	}
	if r := e.do("POST", "/v1/auth/google", "", map[string]string{"id_token": "sub-1|x@y.z|" + hashed, "nonce": "other"}); r.Status != 401 {
		t.Fatalf("bad nonce: %d", r.Status)
	}
	if r := e.do("POST", "/v1/auth/google", "", map[string]string{"id_token": "bad"}); r.Status != 401 {
		t.Fatalf("bad token: %d", r.Status)
	}
	// Google users have no usable password.
	if r := e.do("POST", "/v1/auth/login", "", map[string]string{"login": "jasur1", "password": "!"}); r.Status != 401 {
		t.Fatalf("password login for google user: %d", r.Status)
	}
}

// §5.6 announcements targeting and reads (is_hidden sticky).
func TestAnnouncements(t *testing.T) {
	e := newTestEnv(t)
	pro, sp := e.createUser("ann_pro", proOpts())
	_, sf := e.createUser("ann_free", userOpts{plan: PlanFree, expiry: time.Now(), active: true})
	post := func(body map[string]any) Announcement {
		var a Announcement
		e.must(201, "POST", "/v1/admin/announcements", e.adminToken, body).json(t, &a)
		return a
	}
	all := post(map[string]any{"title": "Hammaga", "body": "b"})
	post(map[string]any{"title": "PRO", "body": "b", "target": "PLAN", "target_plan": 3})
	post(map[string]any{"title": "Shaxsiy", "body": "b", "target": "USER", "target_user_id": pro.ID})
	gone := post(map[string]any{"title": "O'chiriladi", "body": "b"})
	e.must(204, "DELETE", "/v1/admin/announcements/"+gone.ID.String(), e.adminToken, nil)

	n := func(tok string) int {
		var xs []Announcement
		e.must(200, "GET", "/v1/announcements", tok, nil).json(t, &xs)
		return len(xs)
	}
	if n("") != 1 || n(sf.AccessToken) != 1 || n(sp.AccessToken) != 3 {
		t.Fatalf("targeting: anon=%d free=%d pro=%d", n(""), n(sf.AccessToken), n(sp.AccessToken))
	}
	if r := e.do("GET", "/v1/announcements", "invalid-token", nil); r.Status != 401 {
		t.Fatalf("invalid optional token: %d", r.Status)
	}

	e.must(204, "POST", "/v1/announcements/reads", sp.AccessToken, map[string]any{"ids": []string{all.ID.String(), uuid.NewString()}, "hidden": true})
	e.must(204, "POST", "/v1/announcements/reads", sp.AccessToken, map[string]any{"ids": []string{all.ID.String()}, "hidden": false})
	var reads map[string]bool
	e.must(200, "GET", "/v1/announcements/reads", sp.AccessToken, nil).json(t, &reads)
	if len(reads) != 1 || reads[all.ID.String()] != true {
		t.Fatalf("reads: %v", reads)
	}
}

// Admin: only admins; settings PUT writes nulls; payments; audit; stats.
func TestAdminAPI(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("client", proOpts())
	if r := e.do("GET", "/v1/admin/users", s.AccessToken, nil); r.Status != http.StatusForbidden {
		t.Fatalf("non-admin: %d", r.Status)
	}
	if r := e.do("GET", "/v1/admin/users", "", nil); r.Status != 401 {
		t.Fatalf("anon admin: %d", r.Status)
	}
	if r := e.do("POST", "/v1/admin/users", e.adminToken, map[string]any{"login": "client", "password": "secret1",
		"expiry_at": "2030-01-01T00:00:00Z"}); r.Status != 409 || r.code(t) != "LOGIN_TAKEN" {
		t.Fatalf("dup login: %d %s", r.Status, r.Body)
	}

	tg := "@qarznoma_admin"
	e.must(200, "PUT", "/v1/admin/app-settings", e.adminToken, AppSettings{DefaultMaxDevices: 2, TelegramUsername: &tg})
	var pub AppSettings
	e.must(200, "GET", "/v1/app-settings", "", nil).json(t, &pub)
	if pub.DefaultMaxDevices != 2 || pub.TelegramUsername == nil || *pub.TelegramUsername != "qarznoma_admin" || pub.PricePro != nil {
		t.Fatalf("settings: %+v", pub)
	}
	e.must(200, "PUT", "/v1/admin/app-settings", e.adminToken, AppSettings{DefaultMaxDevices: 1})
	e.must(200, "GET", "/v1/app-settings", "", nil).json(t, &pub)
	if pub.TelegramUsername != nil {
		t.Fatal("PUT did not write null")
	}

	pid := uuid.NewString()
	body := map[string]any{"id": pid, "premium_user_id": u.ID, "amount": 15000000, "currency": "UZS",
		"plan_type_index": 3, "months": 3, "method": "CASH", "paid_at": "2026-11-18T10:00:00Z"}
	e.must(201, "POST", "/v1/admin/payments", e.adminToken, body)
	e.must(201, "POST", "/v1/admin/payments", e.adminToken, body) // idempotent
	body["method"] = "BITCOIN"
	body["id"] = uuid.NewString()
	if r := e.do("POST", "/v1/admin/payments", e.adminToken, body); r.Status != 422 {
		t.Fatalf("bad method: %d", r.Status)
	}
	var pays []Payment
	e.must(200, "GET", "/v1/admin/payments?user_id="+u.ID.String(), e.adminToken, nil).json(t, &pays)
	if len(pays) != 1 || pays[0].Amount != 15000000 {
		t.Fatalf("payments: %+v", pays)
	}
	e.must(204, "DELETE", "/v1/admin/payments/"+pid, e.adminToken, nil)

	e.must(200, "PUT", "/v1/devices/dx", s.AccessToken, map[string]string{})
	var seen map[string]string
	e.must(200, "GET", "/v1/admin/last-seen", e.adminToken, nil).json(t, &seen)
	if seen[u.ID.String()] == "" {
		t.Fatalf("last-seen: %v", seen)
	}
	e.must(204, "DELETE", "/v1/admin/users/"+u.ID.String()+"/devices/dx", e.adminToken, nil)

	var stats struct {
		Total  int            `json:"total"`
		ByPlan map[string]int `json:"by_plan"`
	}
	e.must(200, "GET", "/v1/admin/stats", e.adminToken, nil).json(t, &stats)
	if stats.Total != 2 || stats.ByPlan["3"] != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	e.must(204, "DELETE", "/v1/admin/users/"+u.ID.String(), e.adminToken, nil)
	e.must(404, "DELETE", "/v1/admin/users/"+u.ID.String(), e.adminToken, nil)

	var log []AuditEntry
	e.must(200, "GET", "/v1/admin/audit-log?limit=50", e.adminToken, nil).json(t, &log)
	got := map[string]bool{}
	for _, l := range log {
		got[l.Action] = true
	}
	for _, a := range []string{"USER_CREATE", "USER_DELETE", "SETTINGS_UPDATE", "PAYMENT_ADD", "PAYMENT_DELETE", "DEVICE_REMOVE"} {
		if !got[a] {
			t.Fatalf("audit missing %s: %v", a, got)
		}
	}
}

// Error envelope shape and rate limiting.
func TestErrorShapeAndRateLimit(t *testing.T) {
	e := newTestEnv(t)
	r := e.do("GET", "/v1/nope", "", nil)
	if r.Status != 404 || r.code(t) != "NOT_FOUND" {
		t.Fatalf("404 shape: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/v1/auth/login", "", "{not json"); r.Status != 422 || r.code(t) != "VALIDATION" {
		t.Fatalf("bad json: %d %s", r.Status, r.Body)
	}
	e.srv.rlLogin = newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		e.do("POST", "/v1/auth/login", "", map[string]string{"login": "x", "password": "y"})
	}
	if r := e.do("POST", "/v1/auth/login", "", map[string]string{"login": "x", "password": "y"}); r.Status != 429 || r.code(t) != "RATE_LIMITED" {
		t.Fatalf("rate limit: %d %s", r.Status, r.Body)
	}
}
