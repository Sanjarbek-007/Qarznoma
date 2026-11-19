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

	"github.com/Qarznoma/qarznoma-backend/internal/auth"
)

// opt[T] distinguishes "absent" from "null" in JSON bodies.
type opt[T any] struct {
	Set   bool
	Valid bool // false => explicit null
	V     T
}

func (o *opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		return nil
	}
	o.Valid = true
	return json.Unmarshal(b, &o.V)
}

func audit(ctx context.Context, tx pgx.Tx, action string, target *uuid.UUID, label, details string) error {
	_, err := tx.Exec(ctx, `insert into admin_audit_log (action, target_user_id, target_label, details)
		values ($1, $2, $3, $4)`, action, target, label, truncate(details, 2000))
	return err
}

func uuidParam(c *gin.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, validation(name + ": noto'g'ri uuid")
	}
	return id, nil
}

// ── users ───────────────────────────────────────────────────────────────────

func (s *Server) adminListUsers(c *gin.Context) {
	rows, err := s.db.Query(c.Request.Context(), `select `+userCols+` from premium_users order by created_at desc`)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := []*PremiumUser{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			fail(c, err)
			return
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type adminUserBody struct {
	ID                   opt[uuid.UUID] `json:"id"`
	Password             string         `json:"password"`
	Login                opt[string]    `json:"login"`
	FullName             opt[string]    `json:"full_name"`
	Description          opt[string]    `json:"description"`
	PhoneNumber          opt[string]    `json:"phone_number"`
	PremiumPlanTypeIndex opt[int16]     `json:"premium_plan_type_index"`
	StartAt              opt[Time]      `json:"start_at"`
	ExpiryAt             opt[Time]      `json:"expiry_at"`
	IsActive             opt[bool]      `json:"is_active"`
	MaxDevices           opt[int16]     `json:"max_devices"`
}

func (b *adminUserBody) validate() error {
	if b.Login.Set && (!b.Login.Valid || normalizeLogin(b.Login.V) == "" || len(b.Login.V) > 64) {
		return validation("login: 1-64 belgi")
	}
	if b.FullName.Set && (!b.FullName.Valid || len([]rune(b.FullName.V)) > maxName) {
		return validation("full_name noto'g'ri")
	}
	if b.PremiumPlanTypeIndex.Valid && (b.PremiumPlanTypeIndex.V < 0 || b.PremiumPlanTypeIndex.V > 3) {
		return validation("premium_plan_type_index: 0..3")
	}
	if b.MaxDevices.Valid && b.MaxDevices.V < 1 {
		return validation("max_devices: >= 1 yoki null")
	}
	if b.Password != "" && (len([]rune(b.Password)) < 6 || len(b.Password) > 72) {
		return validation("password: 6..72 belgi")
	}
	return nil
}

// POST /v1/admin/users
func (s *Server) adminCreateUser(c *gin.Context) {
	var b adminUserBody
	if err := bindJSON(c, &b, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if err := b.validate(); err != nil {
		fail(c, err)
		return
	}
	if !b.Login.Valid || b.Password == "" || !b.ExpiryAt.Valid {
		fail(c, validation("login, password va expiry_at majburiy"))
		return
	}
	hash, err := auth.HashPassword(b.Password)
	if err != nil {
		fail(c, err)
		return
	}
	id := uuid.New()
	if b.ID.Valid && b.ID.V != uuid.Nil {
		id = b.ID.V
	}
	startAt := time.Now()
	if b.StartAt.Valid {
		startAt = b.StartAt.V.Time
	}
	isActive := !b.IsActive.Set || b.IsActive.V
	var maxDevices *int16
	if b.MaxDevices.Valid {
		maxDevices = &b.MaxDevices.V
	}
	login := normalizeLogin(b.Login.V)
	ctx := c.Request.Context()
	var u *PremiumUser
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			insert into premium_users (id, login, full_name, description, phone_number, premium_plan_type_index,
			                           start_at, expiry_at, is_active, max_devices, signup_source)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'ADMIN')`,
			id, login, b.FullName.V, b.Description.V, b.PhoneNumber.V, b.PremiumPlanTypeIndex.V,
			startAt, b.ExpiryAt.V.Time, isActive, maxDevices)
		if isUniqueViolation(err, "login") {
			return errLoginTaken
		}
		if isUniqueViolation(err, "pkey") {
			return newErr(http.StatusConflict, "ID_TAKEN", "bu id band")
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `insert into auth_credentials (user_id, password_hash) values ($1, $2)`, id, hash); err != nil {
			return err
		}
		if err = audit(ctx, tx, "USER_CREATE", &id, login, fmt.Sprintf("tarif=%d, muddat=%s",
			b.PremiumPlanTypeIndex.V, b.ExpiryAt.V.UTC().Format("2006-01-02"))); err != nil {
			return err
		}
		u, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, u)
}

// PUT /v1/admin/users/{id} — only present fields change; login/password change => 3.7.
func (s *Server) adminUpdateUser(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	var b adminUserBody
	if err := bindJSON(c, &b, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if err := b.validate(); err != nil {
		fail(c, err)
		return
	}
	var hash string
	if b.Password != "" {
		if hash, err = auth.HashPassword(b.Password); err != nil {
			fail(c, err)
			return
		}
	}
	ctx := c.Request.Context()
	var u *PremiumUser
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := scanUser(tx.QueryRow(ctx, `select `+userCols+` from premium_users where id = $1 for update`, id))
		if err != nil {
			return err
		}
		n := *old
		var changes []string
		setStr := func(name string, o opt[string], dst *string) {
			if o.Set && o.Valid && o.V != *dst {
				*dst = o.V
				changes = append(changes, name)
			}
		}
		if b.Login.Valid {
			b.Login.V = normalizeLogin(b.Login.V)
		}
		setStr("login", b.Login, &n.Login)
		setStr("full_name", b.FullName, &n.FullName)
		setStr("description", b.Description, &n.Description)
		setStr("phone_number", b.PhoneNumber, &n.PhoneNumber)
		if b.PremiumPlanTypeIndex.Valid && b.PremiumPlanTypeIndex.V != n.PremiumPlanTypeIndex {
			changes = append(changes, fmt.Sprintf("tarif %d→%d", n.PremiumPlanTypeIndex, b.PremiumPlanTypeIndex.V))
			n.PremiumPlanTypeIndex = b.PremiumPlanTypeIndex.V
		}
		if b.StartAt.Valid && !b.StartAt.V.Equal(n.StartAt.Time) {
			n.StartAt = b.StartAt.V
			changes = append(changes, "start_at")
		}
		if b.ExpiryAt.Valid && !b.ExpiryAt.V.Equal(n.ExpiryAt.Time) {
			changes = append(changes, "muddat→"+b.ExpiryAt.V.UTC().Format("2006-01-02"))
			n.ExpiryAt = b.ExpiryAt.V
		}
		if b.IsActive.Valid && b.IsActive.V != n.IsActive {
			n.IsActive = b.IsActive.V
			changes = append(changes, map[bool]string{true: "blokdan chiqarildi", false: "bloklandi"}[n.IsActive])
		}
		maxChanged := false
		if b.MaxDevices.Set {
			var nv *int16
			if b.MaxDevices.Valid {
				nv = &b.MaxDevices.V
			}
			if (nv == nil) != (n.MaxDevices == nil) || (nv != nil && *nv != *n.MaxDevices) {
				n.MaxDevices = nv
				maxChanged = true
				changes = append(changes, "max_devices")
			}
		}
		_, err = tx.Exec(ctx, `
			update premium_users set login=$2, full_name=$3, description=$4, phone_number=$5,
			       premium_plan_type_index=$6, start_at=$7, expiry_at=$8, is_active=$9, max_devices=$10
			 where id = $1`,
			id, n.Login, n.FullName, n.Description, n.PhoneNumber, n.PremiumPlanTypeIndex,
			n.StartAt.Time, n.ExpiryAt.Time, n.IsActive, n.MaxDevices)
		if isUniqueViolation(err, "login") {
			return errLoginTaken
		}
		if err != nil {
			return err
		}
		credsChanged := n.Login != old.Login
		if hash != "" {
			if _, err = tx.Exec(ctx, `
				insert into auth_credentials (user_id, password_hash) values ($1, $2)
				on conflict (user_id) do update set password_hash = excluded.password_hash`, id, hash); err != nil {
				return err
			}
			credsChanged = true
			changes = append(changes, "parol")
		}
		if credsChanged {
			if err = revokeUserAccess(ctx, tx, id); err != nil {
				return err
			}
		}
		if maxChanged {
			if err = trimDevices(ctx, tx, id); err != nil {
				return err
			}
		}
		if len(changes) > 0 {
			if err = audit(ctx, tx, "USER_EDIT", &id, n.Login, strings.Join(changes, ", ")); err != nil {
				return err
			}
		}
		u, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}

func deleteUserTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, details string) (bool, error) {
	var login string
	err := tx.QueryRow(ctx, `delete from premium_users where id = $1 returning login`, id).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, audit(ctx, tx, "USER_DELETE", &id, login, details)
}

func (s *Server) adminDeleteUser(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	if id == userID(c) {
		fail(c, validation("o'zingizni o'chira olmaysiz"))
		return
	}
	ctx := c.Request.Context()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		found, err := deleteUserTx(ctx, tx, id, "")
		if err == nil && !found {
			return errNotFound
		}
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) adminListDevices(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	ds, err := listDevicesOf(c.Request.Context(), s, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, ds)
}

func (s *Server) adminDeleteDevice(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	did, err := deviceParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var login string
		err := tx.QueryRow(ctx, `
			with d as (delete from user_devices where premium_user_id = $1 and device_id = $2 returning device_name)
			select p.login from premium_users p where p.id = $1 and exists(select 1 from d)`, id, did).Scan(&login)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // idempotent
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, "DEVICE_REMOVE", &id, login, "")
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// PUT /v1/admin/app-settings — every field is written explicitly (null included).
func (s *Server) adminPutAppSettings(c *gin.Context) {
	var a AppSettings
	if err := bindJSON(c, &a, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if a.DefaultMaxDevices < 1 {
		fail(c, validation("default_max_devices: >= 1"))
		return
	}
	if a.TelegramUsername != nil {
		t := strings.TrimPrefix(strings.TrimSpace(*a.TelegramUsername), "@")
		a.TelegramUsername = &t
	}
	ctx := c.Request.Context()
	cols := strings.Split(appSettingsCols, ",")
	sets := make([]string, len(cols))
	for i, col := range cols {
		sets[i] = fmt.Sprintf("%s = $%d", strings.TrimSpace(col), i+1)
	}
	vals := a.fields()
	args := make([]any, len(vals))
	for i, p := range vals { // dereference pointers-to-fields into values
		args[i] = derefField(p)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update app_settings set `+strings.Join(sets, ", ")+` where id`, args...); err != nil {
			return err
		}
		return audit(ctx, tx, "SETTINGS_UPDATE", nil, "", "")
	})
	if err != nil {
		fail(c, err)
		return
	}
	s.getAppSettings(c)
}

func derefField(p any) any {
	switch v := p.(type) {
	case *int16:
		return *v
	case **string:
		return *v
	case **int32:
		return *v
	}
	panic(fmt.Sprintf("unexpected field type %T", p))
}

// ── stats / last-seen / audit ───────────────────────────────────────────────

func (s *Server) adminStats(c *gin.Context) {
	var total, active, expired, blocked, new30 int
	ctx := c.Request.Context()
	err := s.db.QueryRow(ctx, `
		select count(*),
		       count(*) filter (where is_active and expiry_at > now()),
		       count(*) filter (where is_active and expiry_at <= now()),
		       count(*) filter (where not is_active),
		       count(*) filter (where created_at > now() - interval '30 days')
		  from premium_users`).Scan(&total, &active, &expired, &blocked, &new30)
	if err != nil {
		fail(c, err)
		return
	}
	byPlan := map[string]int{"0": 0, "1": 0, "2": 0, "3": 0}
	rows, err := s.db.Query(ctx, `select premium_plan_type_index, count(*) from premium_users group by 1`)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p int16
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			fail(c, err)
			return
		}
		byPlan[strconv.Itoa(int(p))] = n
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "active": active, "expired": expired, "blocked": blocked,
		"by_plan": byPlan, "new_last_30d": new30})
}

func (s *Server) adminLastSeen(c *gin.Context) {
	rows, err := s.db.Query(c.Request.Context(),
		`select premium_user_id, max(last_seen_at) from user_devices group by 1`)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := map[string]Time{}
	for rows.Next() {
		var id uuid.UUID
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			fail(c, err)
			return
		}
		out[id.String()] = Time{t}
	}
	c.JSON(http.StatusOK, out)
}

type AuditEntry struct {
	ID           uuid.UUID  `json:"id"`
	Action       string     `json:"action"`
	TargetUserID *uuid.UUID `json:"target_user_id"`
	TargetLabel  string     `json:"target_label"`
	Details      string     `json:"details"`
	CreatedAt    Time       `json:"created_at"`
}

func (s *Server) adminAuditLog(c *gin.Context) {
	limit := 200
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = min(v, 1000)
	}
	rows, err := s.db.Query(c.Request.Context(), `
		select id, action, target_user_id, target_label, details, created_at
		  from admin_audit_log order by created_at desc limit $1`, limit)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Action, &e.TargetUserID, &e.TargetLabel, &e.Details, &e.CreatedAt.Time); err != nil {
			fail(c, err)
			return
		}
		out = append(out, e)
	}
	c.JSON(http.StatusOK, out)
}

// ── payments ────────────────────────────────────────────────────────────────

type Payment struct {
	ID            uuid.UUID `json:"id"`
	PremiumUserID uuid.UUID `json:"premium_user_id"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	PlanTypeIndex int16     `json:"plan_type_index"`
	Months        int16     `json:"months"`
	Method        string    `json:"method"`
	Note          string    `json:"note"`
	PaidAt        Time      `json:"paid_at"`
}

var paymentMethods = []string{"CASH", "CARD", "PAYME", "CLICK", "GOOGLE_PLAY", "OTHER"}

func (s *Server) adminListPayments(c *gin.Context) {
	ctx := c.Request.Context()
	var rows pgx.Rows
	var err error
	const cols = `id, premium_user_id, amount, currency, plan_type_index, months, method, note, paid_at`
	if v := c.Query("user_id"); v != "" {
		uid, perr := uuid.Parse(v)
		if perr != nil {
			fail(c, validation("user_id: noto'g'ri uuid"))
			return
		}
		rows, err = s.db.Query(ctx, `select `+cols+` from payments where premium_user_id = $1 order by paid_at desc limit 1000`, uid)
	} else {
		rows, err = s.db.Query(ctx, `select `+cols+` from payments order by paid_at desc limit 1000`)
	}
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := []Payment{}
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.PremiumUserID, &p.Amount, &p.Currency, &p.PlanTypeIndex, &p.Months,
			&p.Method, &p.Note, &p.PaidAt.Time); err != nil {
			fail(c, err)
			return
		}
		out = append(out, p)
	}
	c.JSON(http.StatusOK, out)
}

// POST /v1/admin/payments — records only; the admin app extends expiry via PUT /admin/users
// (extending here too would double it).
func (s *Server) adminCreatePayment(c *gin.Context) {
	var p struct {
		ID            *uuid.UUID `json:"id"`
		PremiumUserID uuid.UUID  `json:"premium_user_id"`
		Amount        int64      `json:"amount"`
		Currency      string     `json:"currency"`
		PlanTypeIndex int16      `json:"plan_type_index"`
		Months        int16      `json:"months"`
		Method        string     `json:"method"`
		Note          string     `json:"note"`
		PaidAt        *Time      `json:"paid_at"`
	}
	if err := bindJSON(c, &p, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	switch {
	case p.Amount < 0:
		fail(c, validation("amount >= 0"))
		return
	case p.Months <= 0:
		fail(c, validation("months > 0"))
		return
	case p.PlanTypeIndex < 1 || p.PlanTypeIndex > 3:
		fail(c, validation("plan_type_index: 1..3"))
		return
	case !contains(paymentMethods, p.Method):
		fail(c, validation("method: CASH|CARD|PAYME|CLICK|GOOGLE_PLAY|OTHER"))
		return
	}
	if p.Currency == "" {
		p.Currency = "UZS"
	}
	id := uuid.New()
	if p.ID != nil {
		id = *p.ID
	}
	paidAt := time.Now()
	if p.PaidAt != nil && !p.PaidAt.IsZero() {
		paidAt = p.PaidAt.Time
	}
	ctx := c.Request.Context()
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var login string
		err := tx.QueryRow(ctx, `
			with ins as (
			  insert into payments (id, premium_user_id, amount, currency, plan_type_index, months, method, note, paid_at)
			  values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			  on conflict (id) do nothing returning premium_user_id)
			select p.login from premium_users p join ins on ins.premium_user_id = p.id`,
			id, p.PremiumUserID, p.Amount, p.Currency, p.PlanTypeIndex, p.Months, p.Method, truncate(p.Note, maxText), paidAt).
			Scan(&login)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate id: idempotent
		}
		if isFKViolation(err) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, "PAYMENT_ADD", &p.PremiumUserID, login,
			fmt.Sprintf("%d %s, tarif=%d, %d oy, %s", p.Amount, p.Currency, p.PlanTypeIndex, p.Months, p.Method))
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusCreated)
}

func (s *Server) adminDeletePayment(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var uid uuid.UUID
		var amount int64
		var cur string
		err := tx.QueryRow(ctx, `delete from payments where id = $1 returning premium_user_id, amount, currency`, id).
			Scan(&uid, &amount, &cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var login string
		_ = tx.QueryRow(ctx, `select login from premium_users where id = $1`, uid).Scan(&login)
		return audit(ctx, tx, "PAYMENT_DELETE", &uid, login, fmt.Sprintf("%d %s", amount, cur))
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── announcements (admin) ───────────────────────────────────────────────────

func (s *Server) adminListAnnouncements(c *gin.Context) {
	rows, err := s.db.Query(c.Request.Context(),
		`select `+announcementCols+` from announcements order by created_at desc limit 1000`)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out, err := scanAnnouncements(rows)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) adminCreateAnnouncement(c *gin.Context) {
	var a struct {
		ID           *uuid.UUID `json:"id"`
		Title        string     `json:"title"`
		Body         string     `json:"body"`
		Target       string     `json:"target"`
		TargetPlan   *int16     `json:"target_plan"`
		TargetUserID *uuid.UUID `json:"target_user_id"`
	}
	if err := bindJSON(c, &a, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if a.Target == "" {
		a.Target = "ALL"
	}
	switch {
	case strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Body) == "":
		fail(c, validation("title va body majburiy"))
		return
	case a.Target == "PLAN" && (a.TargetPlan == nil || *a.TargetPlan < 0 || *a.TargetPlan > 3):
		fail(c, validation("target=PLAN uchun target_plan 0..3"))
		return
	case a.Target == "USER" && a.TargetUserID == nil:
		fail(c, validation("target=USER uchun target_user_id majburiy"))
		return
	case !contains([]string{"ALL", "PLAN", "USER"}, a.Target):
		fail(c, validation("target: ALL|PLAN|USER"))
		return
	}
	if a.Target != "PLAN" {
		a.TargetPlan = nil
	}
	if a.Target != "USER" {
		a.TargetUserID = nil
	}
	id := uuid.New()
	if a.ID != nil {
		id = *a.ID
	}
	ctx := c.Request.Context()
	var out Announcement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			insert into announcements (id, title, body, target, target_plan, target_user_id)
			values ($1,$2,$3,$4,$5,$6) returning `+announcementCols,
			id, a.Title, a.Body, a.Target, a.TargetPlan, a.TargetUserID).
			Scan(&out.ID, &out.Title, &out.Body, &out.Target, &out.TargetPlan, &out.TargetUserID, &out.IsDeleted, &out.CreatedAt.Time)
		if isFKViolation(err) {
			return errNotFound
		}
		if isUniqueViolation(err, "pkey") {
			return newErr(http.StatusConflict, "ID_TAKEN", "bu id band")
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, "MESSAGE_SEND", a.TargetUserID, a.Target, truncate(a.Title, 200))
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (s *Server) adminDeleteAnnouncement(c *gin.Context) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var title string
		err := tx.QueryRow(ctx, `update announcements set is_deleted = true where id = $1 and not is_deleted returning title`, id).Scan(&title)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, "MESSAGE_DELETE", nil, "", truncate(title, 200))
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
