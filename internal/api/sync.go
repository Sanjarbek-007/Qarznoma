package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxPushRows = 500
	maxPullRows = 1000
)

var errPlanLimit = newErr(http.StatusConflict, "PLAN_LIMIT", "tarif limiti oshib ketdi")

func tableParam(c *gin.Context) (*syncTable, error) {
	t, ok := syncTables[c.Param("table")]
	if !ok {
		return nil, errNotFound
	}
	return t, nil
}

// decodedRow holds converted values in t.cols order.
type decodedRow []any

// decodePushBody validates every row up-front (3.9: one bad row rejects the chunk).
func decodePushBody(c *gin.Context, t *syncTable, uid uuid.UUID) ([]decodedRow, error) {
	var raw []map[string]json.RawMessage
	if err := bindJSON(c, &raw, maxSyncBody); err != nil {
		return nil, err
	}
	if len(raw) > maxPushRows {
		return nil, newErr(http.StatusRequestEntityTooLarge, "VALIDATION",
			fmt.Sprintf("bir so'rovda ko'pi bilan %d qator", maxPushRows))
	}
	rows := make([]decodedRow, 0, len(raw))
	for i, m := range raw {
		if m == nil {
			return nil, validation(fmt.Sprintf("qator %d: obyekt kutilgan", i))
		}
		r, err := t.decodeRow(m, uid)
		if err != nil {
			return nil, validation(fmt.Sprintf("qator %d: %s", i, err.Error()))
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// decodeRow validates one row and converts it to t.cols order.
// premium_user_id in the body is never trusted; a mismatch rejects the row (3.1).
func (t *syncTable) decodeRow(m map[string]json.RawMessage, uid uuid.UUID) (decodedRow, error) {
	if t.owned {
		if v, ok := m["premium_user_id"]; ok && string(v) != "null" {
			var s string
			if json.Unmarshal(v, &s) != nil || !strings.EqualFold(s, uid.String()) {
				return nil, errors.New("premium_user_id boshqa foydalanuvchiga tegishli")
			}
		}
	}
	r := make(decodedRow, len(t.cols))
	for j, cs := range t.cols {
		v, present := m[cs.name]
		val, err := cs.decodeValue(v, present)
		if err != nil {
			return nil, err
		}
		r[j] = val
	}
	return r, nil
}

func (t *syncTable) colIndex(name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// upsertSQL builds: insert ... on conflict do update ... where <same owner>.
// is_deleted is sticky (3.4); last_updated comes from the sync clock (3.3).
func (t *syncTable) upsertSQL() string {
	names := make([]string, 0, len(t.cols)+2)
	ph := make([]string, 0, len(t.cols)+2)
	sets := make([]string, 0, len(t.cols)+1)
	for i, c := range t.cols {
		names = append(names, c.name)
		ph = append(ph, "$"+strconv.Itoa(i+1))
		switch c.name {
		case "id":
		case "is_deleted":
			sets = append(sets, fmt.Sprintf("is_deleted = %s.is_deleted or excluded.is_deleted", t.name))
		default:
			sets = append(sets, fmt.Sprintf("%s = excluded.%s", c.name, c.name))
		}
	}
	n := len(t.cols)
	names = append(names, "last_updated")
	ph = append(ph, "$"+strconv.Itoa(n+1))
	sets = append(sets, "last_updated = excluded.last_updated")
	where := ""
	if t.owned {
		names = append(names, "premium_user_id")
		ph = append(ph, "$"+strconv.Itoa(n+2))
		where = fmt.Sprintf(" where %s.premium_user_id = excluded.premium_user_id", t.name)
	}
	return fmt.Sprintf("insert into %s (%s) values (%s) on conflict %s do update set %s%s",
		t.name, strings.Join(names, ", "), strings.Join(ph, ", "), t.conflict, strings.Join(sets, ", "), where)
}

// nextClock — global monotonic commit-ordered cursor (3.3). The row lock is held until commit,
// so a reader can never see a smaller last_updated commit after a larger one.
func nextClock(ctx context.Context, tx pgx.Tx) (int64, error) {
	var ms int64
	err := tx.QueryRow(ctx, `
		update sync_clock
		   set last_ms = greatest(last_ms + 1, (extract(epoch from clock_timestamp()) * 1000)::bigint)
		 where id returning last_ms`).Scan(&ms)
	return ms, err
}

// POST /v1/sync/{table}
func (s *Server) syncPush(c *gin.Context) {
	t, err := tableParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	uid := userID(c)
	u, err := getUser(ctx, s.db, uid)
	if err != nil {
		fail(c, err)
		return
	}
	if err := u.writeCheck(time.Now()); err != nil {
		fail(c, err)
		return
	}
	rows, err := decodePushBody(c, t, uid)
	if err != nil {
		fail(c, err)
		return
	}
	if len(rows) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	if err := s.applyPush(ctx, func(tx pgx.Tx) error { return s.pushRows(ctx, tx, t, u, rows) }); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// applyPush runs a write in one transaction and turns data/constraint errors into 422.
// Used by the sync push and by the web endpoints, so both behave the same.
func (s *Server) applyPush(ctx context.Context, fn func(pgx.Tx) error) error {
	err := s.inTx(ctx, fn)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23")) {
		return validation("ma'lumot cheklovi buzildi: " + pe.Message)
	}
	return err
}

func (s *Server) pushRows(ctx context.Context, tx pgx.Tx, t *syncTable, u *PremiumUser, rows []decodedRow) error {
	// Lock the user row: serializes this user's pushes so plan-limit counts are race-free,
	// and re-checks the PRO state inside the transaction.
	var locked PremiumUser
	if err := tx.QueryRow(ctx, `select is_active, premium_plan_type_index, expiry_at
		from premium_users where id = $1 for update`, u.ID).
		Scan(&locked.IsActive, &locked.PremiumPlanTypeIndex, &locked.ExpiryAt.Time); err != nil {
		return err
	}
	if err := locked.writeCheck(time.Now()); err != nil {
		return err
	}

	idIdx, delIdx := t.colIndex("id"), t.colIndex("is_deleted")
	ids := make([]any, len(rows))
	for i, r := range rows {
		ids[i] = r[idIdx]
	}

	var oldDebtorIDs, newDebtorIDs []uuid.UUID
	if t.name == "debts" {
		var err error
		if oldDebtorIDs, newDebtorIDs, err = checkDebtOwnership(ctx, tx, t, u.ID, rows); err != nil {
			return err
		}
	}

	if t.limitKind != "" {
		if err := s.checkPlanLimit(ctx, tx, t, u.ID, locked.PremiumPlanTypeIndex, rows, ids, idIdx, delIdx); err != nil {
			return err
		}
	}

	// Taken last, just before writing, to keep the global clock lock as short as possible.
	clock, err := nextClock(ctx, tx)
	if err != nil {
		return err
	}
	sql := t.upsertSQL()
	b := &pgx.Batch{}
	for _, r := range rows {
		args := append(append([]any{}, r...), clock)
		if t.owned {
			args = append(args, u.ID)
		}
		b.Queue(sql, args...)
	}
	br := tx.SendBatch(ctx, b)
	for i := range rows {
		tag, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return err
		}
		if tag.RowsAffected() != 1 {
			_ = br.Close()
			return validation(fmt.Sprintf("qator %d: bu id boshqa foydalanuvchiga tegishli", i))
		}
	}
	if err := br.Close(); err != nil {
		return err
	}

	if t.name == "debts" {
		return recomputeTotals(ctx, tx, append(oldDebtorIDs, newDebtorIDs...), clock)
	}
	return nil
}

// checkDebtOwnership: every referenced debtor must belong to the user, and every existing debt
// in the chunk must currently belong to the user. Returns old/new debtor ids for 3.5.
func checkDebtOwnership(ctx context.Context, tx pgx.Tx, t *syncTable, uid uuid.UUID, rows []decodedRow) (old, new []uuid.UUID, err error) {
	idIdx, debtorIdx := t.colIndex("id"), t.colIndex("debtor_id")
	ids := make([]uuid.UUID, len(rows))
	set := map[uuid.UUID]struct{}{}
	for i, r := range rows {
		ids[i] = r[idIdx].(uuid.UUID)
		d := r[debtorIdx].(uuid.UUID)
		if _, ok := set[d]; !ok {
			set[d] = struct{}{}
			new = append(new, d)
		}
	}
	var owned int
	if err = tx.QueryRow(ctx, `select count(*) from debtors where id = any($1) and premium_user_id = $2`,
		new, uid).Scan(&owned); err != nil {
		return nil, nil, err
	}
	if owned != len(new) {
		return nil, nil, validation("debtor_id topilmadi yoki boshqa foydalanuvchiga tegishli")
	}
	rs, err := tx.Query(ctx, `
		select d.debtor_id, r.premium_user_id = $2 from debts d join debtors r on r.id = d.debtor_id
		 where d.id = any($1)`, ids, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var d uuid.UUID
		var mine bool
		if err = rs.Scan(&d, &mine); err != nil {
			return nil, nil, err
		}
		if !mine {
			return nil, nil, validation("qarz boshqa foydalanuvchiga tegishli")
		}
		old = append(old, d)
	}
	return old, new, rs.Err()
}

// checkPlanLimit — 7: new live rows must not push the live count over the plan limit.
// Updates and tombstones are always accepted.
func (s *Server) checkPlanLimit(ctx context.Context, tx pgx.Tx, t *syncTable, uid uuid.UUID, plan int16,
	rows []decodedRow, ids []any, idIdx, delIdx int) error {
	settings, err := loadAppSettings(ctx, tx)
	if err != nil {
		return err
	}
	limit := planLimit(settings, plan, t.limitKind)
	if limit == nil {
		return nil
	}
	existing := map[uuid.UUID]bool{}
	er, err := tx.Query(ctx, fmt.Sprintf(`select id from %s where id = any($1)`, t.name), ids)
	if err != nil {
		return err
	}
	for er.Next() {
		var id uuid.UUID
		if err := er.Scan(&id); err != nil {
			er.Close()
			return err
		}
		existing[id] = true
	}
	er.Close()
	if err := er.Err(); err != nil {
		return err
	}
	added := 0
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		id := r[idIdx].(uuid.UUID)
		if !existing[id] && !seen[id] && !r[delIdx].(bool) {
			added++
		}
		seen[id] = true
	}
	if added == 0 {
		return nil
	}
	var live int
	q := `select count(*) from debtors where premium_user_id = $1 and not is_deleted`
	if t.name == "debts" {
		q = `select count(*) from debts d join debtors r on r.id = d.debtor_id
		      where r.premium_user_id = $1 and not d.is_deleted`
	}
	if err := tx.QueryRow(ctx, q, uid).Scan(&live); err != nil {
		return err
	}
	if live+added > *limit {
		return errPlanLimit
	}
	return nil
}

// recomputeTotals — 3.5: total_debts_amount = sum of live debts; bumps last_updated.
func recomputeTotals(ctx context.Context, tx pgx.Tx, debtorIDs []uuid.UUID, clock int64) error {
	if len(debtorIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		update debtors d
		   set total_debts_amount = coalesce((select sum(x.debt_amount) from debts x
		                                       where x.debtor_id = d.id and not x.is_deleted), 0),
		       last_updated = $2
		 where d.id = any($1)`, debtorIDs, clock)
	return err
}

// ── PULL ────────────────────────────────────────────────────────────────────

func (t *syncTable) outCols() []colSpec {
	return append(append([]colSpec{}, t.cols...), t.serverCols...)
}

func (t *syncTable) selectList(alias string) string {
	cs := t.outCols()
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = alias + "." + c.name
	}
	return strings.Join(parts, ", ")
}

func scanSyncRows(rows pgx.Rows, cols []colSpec) ([]map[string]any, error) {
	out := []map[string]any{}
	for rows.Next() {
		targets := make([]any, len(cols))
		getters := make([]func() any, len(cols))
		for i, c := range cols {
			targets[i], getters[i] = c.scanTarget()
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c.name] = getters[i]()
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GET /v1/sync/{table}?since=&after_id=&limit= — keyset (last_updated, id), tombstones included (3.8).
func (s *Server) syncPull(c *gin.Context) {
	t, err := tableParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	since := int64(0)
	if v := c.Query("since"); v != "" {
		if since, err = strconv.ParseInt(v, 10, 64); err != nil {
			fail(c, validation("since: butun son (epoch ms) kutilgan"))
			return
		}
	}
	limit := maxPullRows
	if v := c.Query("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			fail(c, validation("limit: musbat butun son kutilgan"))
			return
		}
		if limit > maxPullRows {
			limit = maxPullRows
		}
	}
	var afterID any
	if v := c.Query("after_id"); v != "" {
		if t.idIsUUID {
			u, err := uuid.Parse(v)
			if err != nil {
				fail(c, validation("after_id: noto'g'ri uuid"))
				return
			}
			afterID = u
		} else {
			afterID = v
		}
	}

	alias := "t"
	from := t.name + " t"
	owner := "t.premium_user_id = $1"
	if !t.owned { // debts: ownership via debtor
		from = "debts t join debtors r on r.id = t.debtor_id"
		owner = "r.premium_user_id = $1"
	}
	cursor := "t.last_updated > $2"
	args := []any{userID(c), since}
	if afterID != nil {
		cursor = "(t.last_updated > $2 or (t.last_updated = $2 and t.id > $3))"
		args = append(args, afterID)
	}
	args = append(args, limit+1)
	q := fmt.Sprintf(`select %s from %s where %s and %s order by t.last_updated, t.id limit $%d`,
		t.selectList(alias), from, owner, cursor, len(args))

	rs, err := s.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		fail(c, err)
		return
	}
	defer rs.Close()
	out, err := scanSyncRows(rs, t.outCols())
	if err != nil {
		fail(c, err)
		return
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"rows": out, "has_more": hasMore})
}

// POST /v1/sync/debtors/by-ids {"ids":[...]} — orphan-debt lookup, tombstones included.
func (s *Server) debtorsByIDs(c *gin.Context) {
	var req struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if err := bindJSON(c, &req, maxSmallBody*4); err != nil {
		fail(c, err)
		return
	}
	if len(req.IDs) > maxPushRows {
		fail(c, validation(fmt.Sprintf("ids ko'pi bilan %d ta", maxPushRows)))
		return
	}
	t := syncTables["debtors"]
	rs, err := s.db.Query(c.Request.Context(),
		fmt.Sprintf(`select %s from debtors t where t.premium_user_id = $1 and t.id = any($2)`, t.selectList("t")),
		userID(c), req.IDs)
	if err != nil {
		fail(c, err)
		return
	}
	defer rs.Close()
	out, err := scanSyncRows(rs, t.outCols())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
