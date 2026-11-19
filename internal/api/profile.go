package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── /v1/me ──────────────────────────────────────────────────────────────────

func (s *Server) getMe(c *gin.Context) {
	u, err := getUser(c.Request.Context(), s.db, userID(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}

// DELETE /v1/me — everything goes via ON DELETE CASCADE (debtors→debts, finance, notes,
// devices, payments, reads, purchases, sessions).
func (s *Server) deleteMe(c *gin.Context) {
	if _, err := s.db.Exec(c.Request.Context(), `delete from premium_users where id = $1`, userID(c)); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── app_settings (6.2) ──────────────────────────────────────────────────────

type AppSettings struct {
	DefaultMaxDevices int16   `json:"default_max_devices"`
	TelegramUsername  *string `json:"telegram_username"`
	SupportPhone      *string `json:"support_phone"`
	SmsText           *string `json:"sms_text"`
	PriceBasic        *string `json:"price_basic"`
	PriceBasic3m      *string `json:"price_basic_3m"`
	PriceBasic6m      *string `json:"price_basic_6m"`
	PriceBasic12m     *string `json:"price_basic_12m"`
	PricePlus         *string `json:"price_plus"`
	PricePlus3m       *string `json:"price_plus_3m"`
	PricePlus6m       *string `json:"price_plus_6m"`
	PricePlus12m      *string `json:"price_plus_12m"`
	PricePro          *string `json:"price_pro"`
	PricePro3m        *string `json:"price_pro_3m"`
	PricePro6m        *string `json:"price_pro_6m"`
	PricePro12m       *string `json:"price_pro_12m"`
	FreeMaxDebtors    *int32  `json:"free_max_debtors"`
	FreeMaxDebts      *int32  `json:"free_max_debts"`
	BasicMaxDebtors   *int32  `json:"basic_max_debtors"`
	BasicMaxDebts     *int32  `json:"basic_max_debts"`
	PlusMaxDebtors    *int32  `json:"plus_max_debtors"`
	PlusMaxDebts      *int32  `json:"plus_max_debts"`
	ProMaxDebtors     *int32  `json:"pro_max_debtors"`
	ProMaxDebts       *int32  `json:"pro_max_debts"`
}

const appSettingsCols = `default_max_devices, telegram_username, support_phone, sms_text,
	price_basic, price_basic_3m, price_basic_6m, price_basic_12m,
	price_plus, price_plus_3m, price_plus_6m, price_plus_12m,
	price_pro, price_pro_3m, price_pro_6m, price_pro_12m,
	free_max_debtors, free_max_debts, basic_max_debtors, basic_max_debts,
	plus_max_debtors, plus_max_debts, pro_max_debtors, pro_max_debts`

func (a *AppSettings) fields() []any {
	return []any{&a.DefaultMaxDevices, &a.TelegramUsername, &a.SupportPhone, &a.SmsText,
		&a.PriceBasic, &a.PriceBasic3m, &a.PriceBasic6m, &a.PriceBasic12m,
		&a.PricePlus, &a.PricePlus3m, &a.PricePlus6m, &a.PricePlus12m,
		&a.PricePro, &a.PricePro3m, &a.PricePro6m, &a.PricePro12m,
		&a.FreeMaxDebtors, &a.FreeMaxDebts, &a.BasicMaxDebtors, &a.BasicMaxDebts,
		&a.PlusMaxDebtors, &a.PlusMaxDebts, &a.ProMaxDebtors, &a.ProMaxDebts}
}

func loadAppSettings(ctx context.Context, q querier) (*AppSettings, error) {
	var a AppSettings
	err := q.QueryRow(ctx, `select `+appSettingsCols+` from app_settings where id`).Scan(a.fields()...)
	return &a, err
}

// GET /v1/app-settings (public) and /v1/admin/app-settings.
func (s *Server) getAppSettings(c *gin.Context) {
	a, err := loadAppSettings(c.Request.Context(), s.db)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, a)
}

// ── devices (5.3) ───────────────────────────────────────────────────────────

type Device struct {
	PremiumUserID uuid.UUID `json:"premium_user_id"`
	DeviceID      string    `json:"device_id"`
	DeviceName    string    `json:"device_name"`
	LastSeenAt    Time      `json:"last_seen_at"`
	CreatedAt     Time      `json:"created_at"`
}

const deviceCols = `premium_user_id, device_id, device_name, last_seen_at, created_at`

func scanDevices(rows pgx.Rows) ([]Device, error) {
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.PremiumUserID, &d.DeviceID, &d.DeviceName, &d.LastSeenAt.Time, &d.CreatedAt.Time); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func listDevicesOf(ctx context.Context, s *Server, uid uuid.UUID) ([]Device, error) {
	rows, err := s.db.Query(ctx, `select `+deviceCols+` from user_devices where premium_user_id = $1
		order by last_seen_at desc`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDevices(rows)
}

func (s *Server) listDevices(c *gin.Context) {
	ds, err := listDevicesOf(c.Request.Context(), s, userID(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, ds)
}

func deviceParam(c *gin.Context) (string, error) {
	d := strings.TrimSpace(c.Param("deviceId"))
	if d == "" || len(d) > 128 {
		return "", validation("device_id noto'g'ri")
	}
	return d, nil
}

func (s *Server) getDevice(c *gin.Context) {
	did, err := deviceParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	var d Device
	err = s.db.QueryRow(c.Request.Context(), `select `+deviceCols+` from user_devices
		where premium_user_id = $1 and device_id = $2`, userID(c), did).
		Scan(&d.PremiumUserID, &d.DeviceID, &d.DeviceName, &d.LastSeenAt.Time, &d.CreatedAt.Time)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(c, errNotFound)
		return
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

// PUT /v1/devices/{id} — upsert; limit applies only to NEW devices; race-free via FOR UPDATE (3.6).
func (s *Server) putDevice(c *gin.Context) {
	did, err := deviceParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	var req struct {
		DeviceName string `json:"device_name"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	uid := userID(c)
	ctx := c.Request.Context()
	var d Device
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var limit int
		err := tx.QueryRow(ctx, `
			select coalesce(p.max_devices, s.default_max_devices, 1)
			  from premium_users p left join app_settings s on s.id
			 where p.id = $1 for update of p`, uid).Scan(&limit)
		if err != nil {
			return err
		}
		var exists bool
		var count int
		if err = tx.QueryRow(ctx, `
			select exists(select 1 from user_devices where premium_user_id = $1 and device_id = $2),
			       (select count(*) from user_devices where premium_user_id = $1)`, uid, did).
			Scan(&exists, &count); err != nil {
			return err
		}
		if !exists && count >= limit {
			return errDeviceLimit
		}
		return tx.QueryRow(ctx, `
			insert into user_devices (premium_user_id, device_id, device_name, last_seen_at)
			values ($1, $2, $3, now())
			on conflict (premium_user_id, device_id)
			do update set device_name = excluded.device_name, last_seen_at = now()
			returning `+deviceCols, uid, did, truncate(req.DeviceName, 200)).
			Scan(&d.PremiumUserID, &d.DeviceID, &d.DeviceName, &d.LastSeenAt.Time, &d.CreatedAt.Time)
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

func (s *Server) deleteDevice(c *gin.Context) {
	did, err := deviceParam(c)
	if err != nil {
		fail(c, err)
		return
	}
	if _, err := s.db.Exec(c.Request.Context(),
		`delete from user_devices where premium_user_id = $1 and device_id = $2`, userID(c), did); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// trimDevices — when max_devices goes down keep the most recently seen N devices (3.6).
func trimDevices(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		delete from user_devices ud
		 where ud.premium_user_id = $1
		   and ud.device_id not in (
		       select device_id from user_devices where premium_user_id = $1
		        order by last_seen_at desc, device_id desc
		        limit (select coalesce(p.max_devices, s.default_max_devices, 1)
		                 from premium_users p left join app_settings s on s.id where p.id = $1))`, uid)
	return err
}

// ── announcements (5.6) ─────────────────────────────────────────────────────

type Announcement struct {
	ID           uuid.UUID  `json:"id"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	Target       string     `json:"target"`
	TargetPlan   *int16     `json:"target_plan"`
	TargetUserID *uuid.UUID `json:"target_user_id"`
	IsDeleted    bool       `json:"is_deleted"`
	CreatedAt    Time       `json:"created_at"`
}

const announcementCols = `id, title, body, target, target_plan, target_user_id, is_deleted, created_at`

func scanAnnouncements(rows pgx.Rows) ([]Announcement, error) {
	out := []Announcement{}
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Target, &a.TargetPlan, &a.TargetUserID,
			&a.IsDeleted, &a.CreatedAt.Time); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GET /v1/announcements — auth optional.
func (s *Server) listAnnouncements(c *gin.Context) {
	ctx := c.Request.Context()
	var rows pgx.Rows
	var err error
	if uid := userID(c); uid != uuid.Nil {
		// Effective plan: expired => FREE (7).
		rows, err = s.db.Query(ctx, `
			select `+announcementCols+` from announcements a
			 where not a.is_deleted and (
			       a.target = 'ALL'
			    or (a.target = 'PLAN' and a.target_plan = (
			           select case when p.expiry_at > now() then p.premium_plan_type_index else 0 end
			             from premium_users p where p.id = $1))
			    or (a.target = 'USER' and a.target_user_id = $1))
			 order by a.created_at desc limit 200`, uid)
	} else {
		rows, err = s.db.Query(ctx, `select `+announcementCols+` from announcements
			where not is_deleted and target = 'ALL' order by created_at desc limit 200`)
	}
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

func (s *Server) getAnnouncementReads(c *gin.Context) {
	rows, err := s.db.Query(c.Request.Context(),
		`select announcement_id, is_hidden from announcement_reads where premium_user_id = $1`, userID(c))
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id uuid.UUID
		var hidden bool
		if err := rows.Scan(&id, &hidden); err != nil {
			fail(c, err)
			return
		}
		out[id.String()] = hidden
	}
	if err := rows.Err(); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// POST /v1/announcements/reads {"ids":[...],"hidden":bool} — is_hidden never goes true→false.
// Unknown announcement ids are skipped silently (deleted on server, still cached on device).
func (s *Server) postAnnouncementReads(c *gin.Context) {
	var req struct {
		IDs    []uuid.UUID `json:"ids"`
		Hidden bool        `json:"hidden"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	if len(req.IDs) > 500 {
		fail(c, validation("ids ko'pi bilan 500 ta"))
		return
	}
	if len(req.IDs) > 0 {
		_, err := s.db.Exec(c.Request.Context(), `
			insert into announcement_reads (premium_user_id, announcement_id, is_hidden, read_at)
			select $1, a.id, $3, $4 from announcements a where a.id = any($2)
			on conflict (premium_user_id, announcement_id)
			do update set is_hidden = announcement_reads.is_hidden or excluded.is_hidden`,
			userID(c), req.IDs, req.Hidden, time.Now())
		if err != nil {
			fail(c, err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}
