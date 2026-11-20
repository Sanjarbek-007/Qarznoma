package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Qarznoma/qarznoma-backend/internal/config"
)

type webPage struct {
	Items   []map[string]any `json:"items"`
	Total   int              `json:"total"`
	Page    int              `json:"page"`
	PerPage int              `json:"per_page"`
}

func (e *testEnv) webList(token, path string) webPage {
	e.t.Helper()
	var p webPage
	e.must(200, "GET", path, token, nil).json(e.t, &p)
	return p
}

func (e *testEnv) webCreateDebtor(token string, body map[string]any) map[string]any {
	e.t.Helper()
	if _, ok := body["repayment_date"]; !ok {
		body["repayment_date"] = "2026-12-31T12:00:00Z"
	}
	var d map[string]any
	e.must(201, "POST", "/v1/debtors", token, body).json(e.t, &d)
	return d
}

func (e *testEnv) webAddDebt(token, debtorID string, amount int64) map[string]any {
	e.t.Helper()
	var d map[string]any
	e.must(201, "POST", "/v1/debtors/"+debtorID+"/debts", token, map[string]any{"debt_amount": amount}).json(e.t, &d)
	return d
}

func (e *testEnv) debtorTotal(token, id string) int64 {
	e.t.Helper()
	var d map[string]any
	e.must(200, "GET", "/v1/debtors/"+id, token, nil).json(e.t, &d)
	return int64(d["total_debts_amount"].(float64))
}

func TestWebDebtorsCRUD(t *testing.T) {
	e := newTestEnv(t)
	u, s := e.createUser("shop1", proOpts())
	tok := s.AccessToken

	// Create: server generates the id, ownership and totals are server-managed.
	ali := e.webCreateDebtor(tok, map[string]any{"full_name": "Ali aka", "phone_number": "+998901234567", "currency_type": 1})
	aliID := ali["id"].(string)
	if ali["premium_user_id"] != u.ID.String() || ali["total_debts_amount"].(float64) != 0 || ali["is_deleted"] != false {
		t.Fatalf("created debtor: %v", ali)
	}

	// Debts update the total.
	d1 := e.webAddDebt(tok, aliID, -5000000)
	e.webAddDebt(tok, aliID, 2000000)
	if got := e.debtorTotal(tok, aliID); got != -3000000 {
		t.Fatalf("total = %d, want -3000000", got)
	}
	if d1["debt_date"] == nil || d1["debtor_id"] != aliID {
		t.Fatalf("debt: %v", d1)
	}

	// PATCH changes only what's sent.
	var upd map[string]any
	e.must(200, "PATCH", "/v1/debtors/"+aliID, tok, map[string]any{"description": "doimiy mijoz"}).json(t, &upd)
	if upd["description"] != "doimiy mijoz" || upd["full_name"] != "Ali aka" || upd["phone_number"] != "+998901234567" {
		t.Fatalf("patched debtor: %v", upd)
	}
	e.must(200, "PATCH", "/v1/debts/"+d1["id"].(string), tok, map[string]any{"debt_amount": -6000000})
	if got := e.debtorTotal(tok, aliID); got != -4000000 {
		t.Fatalf("total after debt patch = %d, want -4000000", got)
	}

	// Moving a debt recomputes both debtors.
	vali := e.webCreateDebtor(tok, map[string]any{"full_name": "Vali"})
	valiID := vali["id"].(string)
	e.must(200, "PATCH", "/v1/debts/"+d1["id"].(string), tok, map[string]any{"debtor_id": valiID})
	if a, v := e.debtorTotal(tok, aliID), e.debtorTotal(tok, valiID); a != 2000000 || v != -6000000 {
		t.Fatalf("after move: ali %d, vali %d", a, v)
	}

	// Debts list: newest first, paged.
	p := e.webList(tok, "/v1/debtors/"+aliID+"/debts")
	if p.Total != 1 || len(p.Items) != 1 {
		t.Fatalf("ali debts: %+v", p)
	}

	// Delete a debt.
	e.must(204, "DELETE", "/v1/debts/"+d1["id"].(string), tok, nil)
	e.must(404, "GET", "/v1/debts/"+d1["id"].(string), tok, nil)
	if v := e.debtorTotal(tok, valiID); v != 0 {
		t.Fatalf("vali total after delete = %d", v)
	}

	// Deleting a debtor tombstones its debts too.
	before := e.count(`select count(*) from debts where debtor_id = $1 and not is_deleted`, aliID)
	if before != 1 {
		t.Fatalf("ali live debts before = %d", before)
	}
	e.must(204, "DELETE", "/v1/debtors/"+aliID, tok, nil)
	e.must(404, "GET", "/v1/debtors/"+aliID, tok, nil)
	e.must(404, "GET", "/v1/debtors/"+aliID+"/debts", tok, nil)
	if n := e.count(`select count(*) from debts where debtor_id = $1 and not is_deleted`, aliID); n != 0 {
		t.Fatalf("ali live debts after delete = %d", n)
	}
	e.must(404, "DELETE", "/v1/debtors/"+aliID, tok, nil)
	if p := e.webList(tok, "/v1/debtors"); p.Total != 1 || p.Items[0]["id"] != valiID {
		t.Fatalf("list after delete: %+v", p)
	}
}

// Browser writes and app sync see the same data.
func TestWebAndSyncShareData(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("shop2", proOpts())
	tok := s.AccessToken

	// Made by the phone via sync → visible in the web list.
	e.must(204, "POST", "/v1/sync/debtors", tok, []any{debtorRow("aaaaaaaa-0000-0000-0000-000000000001")})
	e.must(204, "POST", "/v1/sync/debts", tok, []any{debtRow("bbbbbbbb-0000-0000-0000-000000000001", "aaaaaaaa-0000-0000-0000-000000000001", -100)})
	p := e.webList(tok, "/v1/debtors")
	if p.Total != 1 || p.Items[0]["total_debts_amount"].(float64) != -100 {
		t.Fatalf("web list of synced data: %+v", p)
	}

	// Made in the browser → the phone's next pull (from its last cursor) includes it.
	cursor := int64(0)
	for _, r := range e.pullAll(tok, "debtors") {
		cursor = max(cursor, int64(r["last_updated"].(float64)))
	}
	e.webAddDebt(tok, "aaaaaaaa-0000-0000-0000-000000000001", -50)
	debtors := e.pull(tok, "debtors", cursor, "", 100)
	if len(debtors.Rows) != 1 || debtors.Rows[0]["total_debts_amount"].(float64) != -150 {
		t.Fatalf("phone pull after web write: %+v", debtors)
	}
	debts := e.pullAll(tok, "debts")
	if len(debts) != 2 {
		t.Fatalf("phone sees %d debts, want 2", len(debts))
	}

	// Browser delete reaches the phone as tombstones.
	e.must(204, "DELETE", "/v1/debtors/aaaaaaaa-0000-0000-0000-000000000001", tok, nil)
	for _, r := range e.pullAll(tok, "debts") {
		if r["is_deleted"] != true {
			t.Fatalf("debt not tombstoned for the phone: %v", r)
		}
	}
	// And the debtor can't be revived by an old phone push.
	e.must(204, "POST", "/v1/sync/debtors", tok, []any{debtorRow("aaaaaaaa-0000-0000-0000-000000000001")})
	e.must(404, "GET", "/v1/debtors/aaaaaaaa-0000-0000-0000-000000000001", tok, nil)
}

func TestWebDebtorsAccess(t *testing.T) {
	e := newTestEnv(t)
	_, owner := e.createUser("owner", proOpts())
	_, other := e.createUser("other", proOpts())
	_, free := e.createUser("free", userOpts{plan: PlanFree, expiry: time.Now().AddDate(1, 0, 0), active: true})

	d := e.webCreateDebtor(owner.AccessToken, map[string]any{"full_name": "Ali"})
	id := d["id"].(string)
	debt := e.webAddDebt(owner.AccessToken, id, -10)
	debtID := debt["id"].(string)

	// Someone else's rows look like they don't exist.
	o := other.AccessToken
	e.must(404, "GET", "/v1/debtors/"+id, o, nil)
	e.must(404, "PATCH", "/v1/debtors/"+id, o, map[string]any{"full_name": "x"})
	e.must(404, "DELETE", "/v1/debtors/"+id, o, nil)
	e.must(404, "GET", "/v1/debtors/"+id+"/debts", o, nil)
	e.must(404, "POST", "/v1/debtors/"+id+"/debts", o, map[string]any{"debt_amount": 1})
	e.must(404, "GET", "/v1/debts/"+debtID, o, nil)
	e.must(404, "PATCH", "/v1/debts/"+debtID, o, map[string]any{"debt_amount": 1})
	e.must(404, "DELETE", "/v1/debts/"+debtID, o, nil)
	if p := e.webList(o, "/v1/debtors"); p.Total != 0 {
		t.Fatalf("other user sees %d debtors", p.Total)
	}
	// Can't move your debt onto someone else's debtor.
	mine := e.webCreateDebtor(o, map[string]any{"full_name": "Mine"})
	myDebt := e.webAddDebt(o, mine["id"].(string), -1)
	r := e.must(422, "PATCH", "/v1/debts/"+myDebt["id"].(string), o, map[string]any{"debtor_id": id})
	if r.code(t) != "VALIDATION" {
		t.Fatalf("move to foreign debtor: %s", r.Body)
	}

	// FREE plan: reading works, writing doesn't.
	f := free.AccessToken
	if p := e.webList(f, "/v1/debtors"); p.Total != 0 || p.Items == nil {
		t.Fatalf("free list: %+v", p)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/debtors"}, {"PATCH", "/v1/debtors/" + id}, {"DELETE", "/v1/debtors/" + id},
		{"POST", "/v1/debtors/" + id + "/debts"}, {"PATCH", "/v1/debts/" + debtID}, {"DELETE", "/v1/debts/" + debtID},
	} {
		if r := e.do(c.method, c.path, f, map[string]any{"full_name": "x", "repayment_date": "2026-12-31T00:00:00Z"}); r.Status != 403 || r.code(t) != "FORBIDDEN_NOT_PRO" {
			t.Errorf("FREE %s %s: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}

	// No token.
	e.must(401, "GET", "/v1/debtors", "", nil)
}

func TestWebDebtorsValidation(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("shop3", proOpts())
	tok := s.AccessToken

	cases := []struct {
		name string
		body any
		want int
		code string
	}{
		{"missing full_name", map[string]any{"repayment_date": "2026-12-31T00:00:00Z"}, 422, "VALIDATION"},
		{"blank full_name", map[string]any{"full_name": "   ", "repayment_date": "2026-12-31T00:00:00Z"}, 422, "VALIDATION"},
		{"missing repayment_date", map[string]any{"full_name": "A"}, 422, "VALIDATION"},
		{"unknown field", map[string]any{"full_name": "A", "repayment_date": "2026-12-31T00:00:00Z", "nmae": "x"}, 422, "VALIDATION"},
		{"server field", map[string]any{"full_name": "A", "repayment_date": "2026-12-31T00:00:00Z", "total_debts_amount": 5}, 422, "VALIDATION"},
		{"is_deleted not allowed", map[string]any{"full_name": "A", "repayment_date": "2026-12-31T00:00:00Z", "is_deleted": true}, 422, "VALIDATION"},
		{"bad currency", map[string]any{"full_name": "A", "repayment_date": "2026-12-31T00:00:00Z", "currency_type": 9}, 422, "VALIDATION"},
		{"bad date", map[string]any{"full_name": "A", "repayment_date": "tomorrow"}, 422, "VALIDATION"},
		{"bad id", map[string]any{"id": "nope", "full_name": "A", "repayment_date": "2026-12-31T00:00:00Z"}, 422, "VALIDATION"},
		{"array body", []any{}, 422, ""},
	}
	for _, c := range cases {
		r := e.do("POST", "/v1/debtors", tok, c.body)
		if r.Status != c.want || (c.code != "" && r.code(t) != c.code) {
			t.Errorf("%s: got %d %s", c.name, r.Status, r.Body)
		}
	}

	// Client-chosen id works once, then ID_EXISTS.
	body := map[string]any{"id": "cccccccc-0000-0000-0000-000000000001", "full_name": "A", "repayment_date": "2026-12-31T00:00:00Z"}
	e.must(201, "POST", "/v1/debtors", tok, body)
	if r := e.must(409, "POST", "/v1/debtors", tok, body); r.code(t) != "ID_EXISTS" {
		t.Fatalf("dup id: %s", r.Body)
	}

	e.must(404, "GET", "/v1/debtors/not-a-uuid", tok, nil)
	e.must(422, "POST", "/v1/debtors/cccccccc-0000-0000-0000-000000000001/debts", tok, map[string]any{})
	e.must(422, "POST", "/v1/debtors/cccccccc-0000-0000-0000-000000000001/debts", tok, map[string]any{"debt_amount": "100"})
	e.must(422, "PATCH", "/v1/debtors/cccccccc-0000-0000-0000-000000000001", tok, map[string]any{"full_name": ""})
	e.must(422, "GET", "/v1/debtors?sort=age", tok, nil)
	e.must(422, "GET", "/v1/debtors?per_page=1000", tok, nil)
	e.must(422, "GET", "/v1/debtors?has_debt=maybe", tok, nil)

	// Plan limit applies to the web too.
	if _, err := e.db.Exec(t.Context(), `update app_settings set pro_max_debtors = 1`); err != nil {
		t.Fatal(err)
	}
	if r := e.must(409, "POST", "/v1/debtors", tok, map[string]any{"full_name": "B", "repayment_date": "2026-12-31T00:00:00Z"}); r.code(t) != "PLAN_LIMIT" {
		t.Fatalf("plan limit: %s", r.Body)
	}
}

func TestWebDebtorsListQuery(t *testing.T) {
	e := newTestEnv(t)
	_, s := e.createUser("shop4", proOpts())
	tok := s.AccessToken

	past, future := "2025-01-01T00:00:00Z", "2030-01-01T00:00:00Z"
	mk := func(name, phone, due string, amount int64) string {
		d := e.webCreateDebtor(tok, map[string]any{"full_name": name, "phone_number": phone, "repayment_date": due})
		if amount != 0 {
			e.webAddDebt(tok, d["id"].(string), amount)
		}
		return d["id"].(string)
	}
	mk("Ali Valiyev", "+998 90 123 45 67", past, -300) // overdue
	mk("Bobur", "+998911112233", future, -100)         // owes, not due
	mk("Sardor", "", past, 0)                          // past date but nothing owed
	mk("alisher_100%", "+998935556677", future, -200)  // literal % in the name

	names := func(p webPage) []string {
		var out []string
		for _, it := range p.Items {
			out = append(out, it["full_name"].(string))
		}
		return out
	}
	check := func(path string, want ...string) {
		t.Helper()
		p := e.webList(tok, path)
		got := names(p)
		if fmt.Sprint(got) != fmt.Sprint(want) || p.Total != len(want) {
			t.Errorf("%s: got %v (total %d), want %v", path, got, p.Total, want)
		}
	}

	check("/v1/debtors?sort=name", "Ali Valiyev", "alisher_100%", "Bobur", "Sardor")
	check("/v1/debtors?sort=name&order=desc", "Sardor", "Bobur", "alisher_100%", "Ali Valiyev")
	check("/v1/debtors?sort=total&order=asc", "Ali Valiyev", "alisher_100%", "Bobur", "Sardor")
	check("/v1/debtors?q=ali&sort=name", "Ali Valiyev", "alisher_100%")
	check("/v1/debtors?q=100%25", "alisher_100%")   // % is matched literally
	check("/v1/debtors?q=_", "alisher_100%")        // so is _
	check("/v1/debtors?q=901234567", "Ali Valiyev") // digits ignore spaces in the stored phone
	check("/v1/debtors?q=91%20111", "Bobur")        // and in the query
	check("/v1/debtors?has_debt=true&sort=name", "Ali Valiyev", "alisher_100%", "Bobur")
	check("/v1/debtors?overdue=true", "Ali Valiyev")

	// Pages.
	p1 := e.webList(tok, "/v1/debtors?sort=name&per_page=3")
	p2 := e.webList(tok, "/v1/debtors?sort=name&per_page=3&page=2")
	p3 := e.webList(tok, "/v1/debtors?sort=name&per_page=3&page=3")
	if len(p1.Items) != 3 || len(p2.Items) != 1 || len(p3.Items) != 0 || p1.Total != 4 || p2.Page != 2 || p2.PerPage != 3 {
		t.Fatalf("paging: %d/%d/%d total %d", len(p1.Items), len(p2.Items), len(p3.Items), p1.Total)
	}
	if p2.Items[0]["full_name"] != "Sardor" {
		t.Fatalf("page 2: %v", names(p2))
	}
}

// CORS: preflight requests (which match no route) get the headers for allowed origins only.
func TestCORSPreflight(t *testing.T) {
	cfg := &config.Config{
		JWTSecret: []byte("test-secret-test-secret-test-secret!"),
		WebOrigin: "https://app.example.uz, http://localhost:5173/",
	}
	h := New(cfg, nil).Handler()
	preflight := func(origin, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("OPTIONS", path, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "PATCH")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for _, origin := range []string{"https://app.example.uz", "http://localhost:5173"} {
		w := preflight(origin, "/v1/debtors/123")
		if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("%s: %d %v", origin, w.Code, w.Header())
		}
		if m := w.Header().Get("Access-Control-Allow-Methods"); m == "" || !containsWord(m, "PATCH") || !containsWord(m, "DELETE") {
			t.Errorf("%s: methods %q", origin, m)
		}
	}
	if w := preflight("https://evil.example", "/v1/debtors"); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("unknown origin got CORS headers")
	}
	// The deletion page's endpoint too.
	if w := preflight("https://app.example.uz", "/v1/account-deletion-requests"); w.Code != http.StatusNoContent {
		t.Errorf("deletion preflight: %d", w.Code)
	}
}

func containsWord(list, w string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == w {
			return true
		}
	}
	return false
}
