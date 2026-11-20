package api

// REST endpoints for debtors and debts, for browser clients that don't keep an offline database.
//
// Every write builds a normal sync row and goes through pushRows, the same path as
// POST /v1/sync/{table}. So ownership, the PRO check, plan limits, debtor totals and the
// last_updated clock behave exactly as in sync, and phones pick up browser changes on their
// next pull.
//
// Reads need only a logged-in user; writes need an active PRO plan (like sync).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	webDefaultPerPage = 50
	webMaxPerPage     = 200
)

// Fields a browser may set. Server-managed fields (premium_user_id, total_debts_amount,
// last_updated, is_deleted) are not accepted; deleting is DELETE.
var (
	debtorWritable = []string{"full_name", "description", "phone_number", "currency_type", "repayment_date"}
	debtWritable   = []string{"debt_amount", "debt_description", "debt_date"}
)

type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type page struct {
	Items   []map[string]any `json:"items"`
	Total   int              `json:"total"`
	Page    int              `json:"page"`
	PerPage int              `json:"per_page"`
}

// ── helpers ────────────────────────────────────────────────────────────────

func pathUUID(c *gin.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, errNotFound
	}
	return id, nil
}

func pageParams(c *gin.Context) (pageNo, perPage int, err error) {
	pageNo, perPage = 1, webDefaultPerPage
	if v := c.Query("page"); v != "" {
		if pageNo, err = strconv.Atoi(v); err != nil || pageNo < 1 {
			return 0, 0, validation("page: 1 yoki undan katta butun son")
		}
	}
	if v := c.Query("per_page"); v != "" {
		if perPage, err = strconv.Atoi(v); err != nil || perPage < 1 || perPage > webMaxPerPage {
			return 0, 0, validation(fmt.Sprintf("per_page: 1..%d", webMaxPerPage))
		}
	}
	return pageNo, perPage, nil
}

func boolParam(c *gin.Context, name string) (bool, error) {
	switch c.Query(name) {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, validation(name + ": true yoki false")
}

func queryOne(ctx context.Context, q rowQuerier, cols []colSpec, sql string, args ...any) (map[string]any, error) {
	rs, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out, err := scanSyncRows(rs, cols)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errNotFound
	}
	return out[0], nil
}

// loadDebtor returns a live debtor of the user, or 404.
func loadDebtor(ctx context.Context, q rowQuerier, uid, id uuid.UUID) (map[string]any, error) {
	t := syncTables["debtors"]
	return queryOne(ctx, q, t.outCols(), `select `+t.selectList("d")+` from debtors d
		where d.id = $1 and d.premium_user_id = $2 and not d.is_deleted`, id, uid)
}

// loadDebt returns a live debt of a live debtor of the user, or 404.
func loadDebt(ctx context.Context, q rowQuerier, uid, id uuid.UUID) (map[string]any, error) {
	t := syncTables["debts"]
	return queryOne(ctx, q, t.outCols(), `select `+t.selectList("d")+` from debts d
		join debtors r on r.id = d.debtor_id
		where d.id = $1 and r.premium_user_id = $2 and not d.is_deleted and not r.is_deleted`, id, uid)
}

// readBody reads a JSON object and rejects keys outside allowed (catches typos and
// attempts to set server-managed fields).
func readBody(c *gin.Context, allowed ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := bindJSON(c, &m, maxSmallBody); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, validation("JSON obyekt kutilgan")
	}
	var unknown []string
	for k := range m {
		if !contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, validation(fmt.Sprintf("noma'lum maydon(lar): %s; ruxsat etilgan: %s",
			strings.Join(unknown, ", "), strings.Join(allowed, ", ")))
	}
	return m, nil
}

// toRaw turns a row read from the DB back into the JSON-ish map decodeRow expects.
func toRaw(m map[string]any) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, nil
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// requireName rejects a blank full_name. Stricter than sync (which accepts ""), so the Android
// app's existing pushes keep working while browser forms can't create nameless debtors.
func requireName(m map[string]json.RawMessage) error {
	var name string
	if v, ok := m["full_name"]; ok && json.Unmarshal(v, &name) == nil && strings.TrimSpace(name) == "" {
		return validation("full_name bo'sh bo'lmasin")
	}
	return nil
}

// writableUser loads the user and applies the PRO write rule before any work.
func (s *Server) writableUser(c *gin.Context) (*PremiumUser, error) {
	u, err := getUser(c.Request.Context(), s.db, userID(c))
	if err != nil {
		return nil, err
	}
	if err := u.writeCheck(time.Now()); err != nil {
		return nil, err
	}
	return u, nil
}

// lockUser serializes this user's writes, so a read-modify-write (PATCH) can't lose a
// concurrent change. pushRows takes the same lock again (re-entrant within the transaction).
func lockUser(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	_, err := tx.Exec(ctx, `select 1 from premium_users where id = $1 for update`, uid)
	return err
}

// pushOne validates one row and writes it through the sync path inside tx.
func (s *Server) pushOne(ctx context.Context, tx pgx.Tx, u *PremiumUser, table string, m map[string]json.RawMessage) error {
	t := syncTables[table]
	r, err := t.decodeRow(m, u.ID)
	if err != nil {
		return validation(err.Error())
	}
	return s.pushRows(ctx, tx, t, u, []decodedRow{r})
}

// newRowID takes the client's id (lets an app create rows offline with its own ids) or makes one.
// An id that already exists in the table is rejected, so POST never silently updates.
func (s *Server) newRowID(ctx context.Context, table string, m map[string]json.RawMessage) (uuid.UUID, error) {
	raw, ok := m["id"]
	if !ok || string(raw) == "null" {
		return uuid.New(), nil
	}
	var str string
	if json.Unmarshal(raw, &str) != nil {
		return uuid.Nil, validation("id: uuid matni kutilgan")
	}
	id, err := uuid.Parse(str)
	if err != nil {
		return uuid.Nil, validation("id: noto'g'ri uuid")
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `select exists(select 1 from `+table+` where id = $1)`, id).Scan(&exists); err != nil {
		return uuid.Nil, err
	}
	if exists {
		return uuid.Nil, newErr(http.StatusConflict, "ID_EXISTS", "bu id allaqachon mavjud")
	}
	return id, nil
}

// ── debtors ────────────────────────────────────────────────────────────────

var debtorSorts = map[string]string{
	"name":           "lower(d.full_name)",
	"total":          "d.total_debts_amount",
	"repayment_date": "d.repayment_date",
	"updated":        "d.last_updated",
}

// GET /v1/debtors?q=&has_debt=&overdue=&sort=&order=&page=&per_page=
func (s *Server) webListDebtors(c *gin.Context) {
	pageNo, perPage, err := pageParams(c)
	if err != nil {
		fail(c, err)
		return
	}
	sortKey := c.DefaultQuery("sort", "updated")
	sortCol, ok := debtorSorts[sortKey]
	if !ok {
		fail(c, validation("sort: name | total | repayment_date | updated"))
		return
	}
	order := c.Query("order")
	if order == "" {
		order = "desc"
		if sortKey == "name" || sortKey == "repayment_date" {
			order = "asc"
		}
	}
	if order != "asc" && order != "desc" {
		fail(c, validation("order: asc | desc"))
		return
	}
	hasDebt, err := boolParam(c, "has_debt")
	if err != nil {
		fail(c, err)
		return
	}
	overdue, err := boolParam(c, "overdue")
	if err != nil {
		fail(c, err)
		return
	}

	where := []string{"d.premium_user_id = $1", "not d.is_deleted"}
	args := []any{userID(c)}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if len([]rune(q)) > 100 {
			fail(c, validation("q: 100 belgidan oshmasin"))
			return
		}
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		cond := fmt.Sprintf("d.full_name ilike %[1]s or d.phone_number ilike %[1]s", arg(like))
		// "90 123 45" also finds "+998901234567": compare digits only.
		if digits := strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, q); len(digits) >= 3 {
			cond += fmt.Sprintf(" or regexp_replace(d.phone_number, '[^0-9]', '', 'g') like %s", arg("%"+digits+"%"))
		}
		where = append(where, "("+cond+")")
	}
	if hasDebt || overdue {
		where = append(where, "d.total_debts_amount <> 0")
	}
	if overdue {
		where = append(where, "d.repayment_date < now()")
	}
	whereSQL := strings.Join(where, " and ")

	ctx := c.Request.Context()
	var total int
	if err := s.db.QueryRow(ctx, `select count(*) from debtors d where `+whereSQL, args...).Scan(&total); err != nil {
		fail(c, err)
		return
	}
	t := syncTables["debtors"]
	limit, offset := arg(perPage), arg((pageNo-1)*perPage)
	rs, err := s.db.Query(ctx, fmt.Sprintf(`select %s from debtors d where %s order by %s %s, d.id %s limit %s offset %s`,
		t.selectList("d"), whereSQL, sortCol, order, order, limit, offset), args...)
	if err != nil {
		fail(c, err)
		return
	}
	defer rs.Close()
	items, err := scanSyncRows(rs, t.outCols())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, page{Items: items, Total: total, Page: pageNo, PerPage: perPage})
}

// GET /v1/debtors/{id}
func (s *Server) webGetDebtor(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebtor(c.Request.Context(), s.db, userID(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// POST /v1/debtors
func (s *Server) webCreateDebtor(c *gin.Context) {
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	m, err := readBody(c, append([]string{"id"}, debtorWritable...)...)
	if err == nil {
		err = requireName(m)
	}
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	id, err := s.newRowID(ctx, "debtors", m)
	if err != nil {
		fail(c, err)
		return
	}
	m["id"] = rawJSON(id)
	m["is_deleted"] = rawJSON(false)
	m["updated_at_client"] = rawJSON(time.Now().UnixMilli())
	if err := s.applyPush(ctx, func(tx pgx.Tx) error { return s.pushOne(ctx, tx, u, "debtors", m) }); err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebtor(ctx, s.db, u.ID, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, d)
}

// PATCH /v1/debtors/{id} — only the fields sent are changed.
func (s *Server) webUpdateDebtor(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	patch, err := readBody(c, debtorWritable...)
	if err == nil {
		err = requireName(patch)
	}
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.applyPush(ctx, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, u.ID); err != nil {
			return err
		}
		cur, err := loadDebtor(ctx, tx, u.ID, id)
		if err != nil {
			return err
		}
		m, err := toRaw(cur)
		if err != nil {
			return err
		}
		for k, v := range patch {
			m[k] = v
		}
		m["updated_at_client"] = rawJSON(time.Now().UnixMilli())
		return s.pushOne(ctx, tx, u, "debtors", m)
	})
	if err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebtor(ctx, s.db, u.ID, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// DELETE /v1/debtors/{id} — tombstones the debtor and all its debts in one transaction.
func (s *Server) webDeleteDebtor(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.applyPush(ctx, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, u.ID); err != nil {
			return err
		}
		cur, err := loadDebtor(ctx, tx, u.ID, id)
		if err != nil {
			return err
		}
		// Debts first, so their totals recompute is followed by the debtor's own tombstone.
		dt := syncTables["debts"]
		rs, err := tx.Query(ctx, `select `+dt.selectList("d")+` from debts d where d.debtor_id = $1 and not d.is_deleted`, id)
		if err != nil {
			return err
		}
		debts, err := scanSyncRows(rs, dt.outCols())
		rs.Close()
		if err != nil {
			return err
		}
		if len(debts) > 0 {
			rows := make([]decodedRow, 0, len(debts))
			for _, d := range debts {
				d["is_deleted"] = true
				m, err := toRaw(d)
				if err != nil {
					return err
				}
				r, err := dt.decodeRow(m, u.ID)
				if err != nil {
					return err
				}
				rows = append(rows, r)
			}
			if err := s.pushRows(ctx, tx, dt, u, rows); err != nil {
				return err
			}
		}
		cur["is_deleted"] = true
		m, err := toRaw(cur)
		if err != nil {
			return err
		}
		m["updated_at_client"] = rawJSON(time.Now().UnixMilli())
		return s.pushOne(ctx, tx, u, "debtors", m)
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── debts ──────────────────────────────────────────────────────────────────

// GET /v1/debtors/{id}/debts?page=&per_page= — newest debt_date first.
func (s *Server) webListDebts(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	pageNo, perPage, err := pageParams(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	if _, err := loadDebtor(ctx, s.db, userID(c), id); err != nil {
		fail(c, err)
		return
	}
	var total int
	if err := s.db.QueryRow(ctx, `select count(*) from debts where debtor_id = $1 and not is_deleted`, id).Scan(&total); err != nil {
		fail(c, err)
		return
	}
	t := syncTables["debts"]
	rs, err := s.db.Query(ctx, `select `+t.selectList("d")+` from debts d
		where d.debtor_id = $1 and not d.is_deleted
		order by d.debt_date desc, d.id desc limit $2 offset $3`, id, perPage, (pageNo-1)*perPage)
	if err != nil {
		fail(c, err)
		return
	}
	defer rs.Close()
	items, err := scanSyncRows(rs, t.outCols())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, page{Items: items, Total: total, Page: pageNo, PerPage: perPage})
}

// POST /v1/debtors/{id}/debts — debt_date defaults to now.
func (s *Server) webCreateDebt(c *gin.Context) {
	debtorID, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	m, err := readBody(c, append([]string{"id"}, debtWritable...)...)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	if _, err := loadDebtor(ctx, s.db, u.ID, debtorID); err != nil {
		fail(c, err)
		return
	}
	id, err := s.newRowID(ctx, "debts", m)
	if err != nil {
		fail(c, err)
		return
	}
	m["id"] = rawJSON(id)
	m["debtor_id"] = rawJSON(debtorID)
	m["is_deleted"] = rawJSON(false)
	if v, ok := m["debt_date"]; !ok || string(v) == "null" {
		m["debt_date"] = rawJSON(time.Now().UTC().Format(time.RFC3339))
	}
	err = s.applyPush(ctx, func(tx pgx.Tx) error {
		// Re-check under the transaction: the debtor may have been deleted meanwhile.
		if _, err := loadDebtor(ctx, tx, u.ID, debtorID); err != nil {
			return err
		}
		return s.pushOne(ctx, tx, u, "debts", m)
	})
	if err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebt(ctx, s.db, u.ID, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, d)
}

// GET /v1/debts/{id}
func (s *Server) webGetDebt(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebt(c.Request.Context(), s.db, userID(c), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// PATCH /v1/debts/{id} — only the fields sent are changed. debtor_id moves the debt to
// another of your debtors (both totals are recomputed).
func (s *Server) webUpdateDebt(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	patch, err := readBody(c, append([]string{"debtor_id"}, debtWritable...)...)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.applyPush(ctx, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, u.ID); err != nil {
			return err
		}
		cur, err := loadDebt(ctx, tx, u.ID, id)
		if err != nil {
			return err
		}
		if v, ok := patch["debtor_id"]; ok {
			var str string
			newID, perr := uuid.Nil, json.Unmarshal(v, &str)
			if perr == nil {
				newID, perr = uuid.Parse(str)
			}
			if perr != nil {
				return validation("debtor_id: noto'g'ri uuid")
			}
			if _, err := loadDebtor(ctx, tx, u.ID, newID); err != nil {
				if errors.Is(err, errNotFound) {
					return validation("debtor_id: qarzdor topilmadi")
				}
				return err
			}
		}
		m, err := toRaw(cur)
		if err != nil {
			return err
		}
		for k, v := range patch {
			m[k] = v
		}
		return s.pushOne(ctx, tx, u, "debts", m)
	})
	if err != nil {
		fail(c, err)
		return
	}
	d, err := loadDebt(ctx, s.db, u.ID, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// DELETE /v1/debts/{id}
func (s *Server) webDeleteDebt(c *gin.Context) {
	id, err := pathUUID(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	u, err := s.writableUser(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.applyPush(ctx, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, u.ID); err != nil {
			return err
		}
		cur, err := loadDebt(ctx, tx, u.ID, id)
		if err != nil {
			return err
		}
		cur["is_deleted"] = true
		m, err := toRaw(cur)
		if err != nil {
			return err
		}
		return s.pushOne(ctx, tx, u, "debts", m)
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
