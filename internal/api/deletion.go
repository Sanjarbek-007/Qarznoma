package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// POST /v1/account-deletion-requests — public web form. Does NOT delete: records a PENDING
// request; the admin calls the owner and approves (5.1, 5.5).
func (s *Server) createDeletionRequest(c *gin.Context) {
	var req struct {
		Login    string `json:"login"`
		Password string `json:"password"`
		Phone    string `json:"phone"`
		Lang     string `json:"lang"`
	}
	if err := bindJSON(c, &req, maxSmallBody); err != nil {
		fail(c, err)
		return
	}
	login := normalizeLogin(req.Login)
	if !s.rlDeletion.allow(clientIP(c)+"|"+login) || !s.rlDeletion.allow("ip|"+clientIP(c)) {
		fail(c, errRateLimited)
		return
	}
	digits := 0
	for _, r := range req.Phone {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	if digits < 7 || len(req.Phone) > 32 {
		fail(c, newErr(http.StatusBadRequest, "VALIDATION", "telefon raqami noto'g'ri"))
		return
	}
	lang := req.Lang
	if !contains([]string{"uz", "uz-Cyrl", "ru", "en"}, lang) {
		lang = "uz"
	}
	ctx := c.Request.Context()
	uid, err := s.checkCredentials(ctx, login, req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	_, err = s.db.Exec(ctx, `
		insert into account_deletion_requests (premium_user_id, login, contact_phone, account_phone, lang, ip, user_agent)
		select p.id, p.login, $2, p.phone_number, $3, $4, $5 from premium_users p where p.id = $1`,
		uid, strings.TrimSpace(req.Phone), lang, clientIP(c), truncate(c.Request.UserAgent(), 300))
	if isUniqueViolation(err, "") {
		fail(c, errRequestExists)
		return
	}
	if err != nil {
		fail(c, err)
		return
	}
	// TODO: notify the admin (Telegram bot / email) — HANDOFF 5.1 TAVSIYA.
	c.JSON(http.StatusAccepted, gin.H{"status": "PENDING"})
}

type DeletionRequest struct {
	ID            uuid.UUID `json:"id"`
	PremiumUserID uuid.UUID `json:"premium_user_id"`
	Login         string    `json:"login"`
	ContactPhone  string    `json:"contact_phone"`
	AccountPhone  string    `json:"account_phone"`
	Lang          string    `json:"lang"`
	Status        string    `json:"status"`
	AdminNote     string    `json:"admin_note"`
	IP            string    `json:"ip"`
	CreatedAt     Time      `json:"created_at"`
	ResolvedAt    NullTime  `json:"resolved_at"`
}

const deletionCols = `id, premium_user_id, login, contact_phone, account_phone, lang, status, admin_note, ip, created_at, resolved_at`

func scanDeletion(row pgx.Row) (*DeletionRequest, error) {
	var d DeletionRequest
	var resolved *pgTime
	err := row.Scan(&d.ID, &d.PremiumUserID, &d.Login, &d.ContactPhone, &d.AccountPhone, &d.Lang,
		&d.Status, &d.AdminNote, &d.IP, &d.CreatedAt.Time, &resolved)
	if resolved != nil {
		d.ResolvedAt = NullTime{Time: *resolved, Valid: true}
	}
	return &d, err
}

func (s *Server) adminListDeletionRequests(c *gin.Context) {
	ctx := c.Request.Context()
	var rows pgx.Rows
	var err error
	if st := c.Query("status"); st != "" {
		rows, err = s.db.Query(ctx, `select `+deletionCols+` from account_deletion_requests
			where status = $1 order by created_at desc limit 1000`, st)
	} else {
		rows, err = s.db.Query(ctx, `select `+deletionCols+` from account_deletion_requests
			order by created_at desc limit 1000`)
	}
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := []*DeletionRequest{}
	for rows.Next() {
		d, err := scanDeletion(rows)
		if err != nil {
			fail(c, err)
			return
		}
		out = append(out, d)
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) resolveDeletion(c *gin.Context, approve bool) {
	id, err := uuidParam(c, "id")
	if err != nil {
		fail(c, err)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength != 0 {
		if err := bindJSON(c, &body, maxSmallBody); err != nil {
			fail(c, err)
			return
		}
	}
	status := map[bool]string{true: "APPROVED", false: "REJECTED"}[approve]
	ctx := c.Request.Context()
	var out *DeletionRequest
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var uid uuid.UUID
		var login, cur string
		err := tx.QueryRow(ctx, `select premium_user_id, login, status from account_deletion_requests
			where id = $1 for update`, id).Scan(&uid, &login, &cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if cur != "PENDING" {
			return newErr(http.StatusConflict, "NOT_PENDING", "so'rov allaqachon ko'rib chiqilgan")
		}
		if approve {
			found, err := deleteUserTx(ctx, tx, uid, "web so'rovi, tasdiqlandi")
			if err != nil {
				return err
			}
			if !found { // already gone — still mark APPROVED
				if err = audit(ctx, tx, "USER_DELETE", &uid, login, "web so'rovi, tasdiqlandi (akkaunt allaqachon yo'q)"); err != nil {
					return err
				}
			}
		} else if err = audit(ctx, tx, "DELETE_REQUEST_REJECT", &uid, login, truncate(body.Note, 500)); err != nil {
			return err
		}
		out, err = scanDeletion(tx.QueryRow(ctx, `
			update account_deletion_requests set status = $2, admin_note = $3, resolved_at = now()
			 where id = $1 returning `+deletionCols, id, status, truncate(body.Note, 2000)))
		return err
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) adminApproveDeletion(c *gin.Context) { s.resolveDeletion(c, true) }
func (s *Server) adminRejectDeletion(c *gin.Context)  { s.resolveDeletion(c, false) }
