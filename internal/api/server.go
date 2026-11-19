// Package api implements the Qarznoma HTTP API (/v1) on top of Gin + pgx.
package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Qarznoma/qarznoma-backend/internal/auth"
	"github.com/Qarznoma/qarznoma-backend/internal/config"
)

type Server struct {
	cfg    *config.Config
	db     *pgxpool.Pool
	tokens *auth.Issuer
	google GoogleVerifier

	rlLogin, rlRegister, rlLoginCheck, rlGoogle, rlDeletion, rlSync *rateLimiter
}

func New(cfg *config.Config, db *pgxpool.Pool) *Server {
	s := &Server{
		cfg:          cfg,
		db:           db,
		tokens:       auth.NewIssuer(cfg.JWTSecret, cfg.AccessTTL),
		rlLogin:      newRateLimiter(10, time.Minute),
		rlRegister:   newRateLimiter(5, time.Hour),
		rlLoginCheck: newRateLimiter(60, time.Minute),
		rlGoogle:     newRateLimiter(20, time.Minute),
		rlDeletion:   newRateLimiter(5, time.Hour),
		rlSync:       newRateLimiter(300, time.Minute),
	}
	if cfg.GoogleWebClientID != "" {
		s.google = newGoogleVerifier(cfg.GoogleWebClientID)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if s.cfg.TrustProxy {
		_ = r.SetTrustedProxies([]string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"})
	} else {
		_ = r.SetTrustedProxies(nil)
	}
	r.Use(requestID(), accessLog(), recovery())
	r.NoRoute(func(c *gin.Context) { fail(c, errNotFound) })
	r.NoMethod(func(c *gin.Context) { fail(c, errNotFound) })

	r.GET("/healthz", s.healthz)
	if s.cfg.WebDir != "" {
		r.StaticFile("/privacy", filepath.Join(s.cfg.WebDir, "privacy-policy.html"))
		r.StaticFile("/privacy-policy.html", filepath.Join(s.cfg.WebDir, "privacy-policy.html"))
		r.StaticFile("/delete-account", filepath.Join(s.cfg.WebDir, "delete-account.html"))
		r.StaticFile("/delete-account.html", filepath.Join(s.cfg.WebDir, "delete-account.html"))
	}

	v1 := r.Group("/v1")
	v1.Use(s.cors())

	// ── public ──
	v1.POST("/auth/login", s.login)
	v1.POST("/auth/refresh", s.refresh)
	v1.POST("/auth/logout", s.logout)
	v1.POST("/auth/register", s.register)
	v1.POST("/auth/google", s.googleSignIn)
	v1.GET("/auth/login-available", s.loginAvailable)
	v1.GET("/auth/suggest-login", s.suggestLogin)
	v1.POST("/account-deletion-requests", s.createDeletionRequest)
	v1.GET("/app-settings", s.getAppSettings)
	v1.GET("/announcements", s.authOptional(), s.listAnnouncements)

	// ── user ──
	u := v1.Group("", s.authRequired())
	u.GET("/me", s.getMe)
	u.DELETE("/me", s.deleteMe)
	u.GET("/announcements/reads", s.getAnnouncementReads)
	u.POST("/announcements/reads", s.postAnnouncementReads)

	u.GET("/devices", s.listDevices)
	u.GET("/devices/:deviceId", s.getDevice)
	u.PUT("/devices/:deviceId", s.putDevice)
	u.DELETE("/devices/:deviceId", s.deleteDevice)

	sync := u.Group("/sync", s.syncRateLimit())
	sync.POST("/debtors/by-ids", s.debtorsByIDs)
	sync.POST("/:table", s.syncPush)
	sync.GET("/:table", s.syncPull)

	u.POST("/billing/google/verify", s.billingVerify)
	v1.POST("/billing/google/rtdn", s.billingRTDN)

	// ── admin ──
	a := u.Group("/admin", s.adminOnly())
	a.GET("/users", s.adminListUsers)
	a.POST("/users", s.adminCreateUser)
	a.PUT("/users/:id", s.adminUpdateUser)
	a.DELETE("/users/:id", s.adminDeleteUser)
	a.GET("/users/:id/devices", s.adminListDevices)
	a.DELETE("/users/:id/devices/:deviceId", s.adminDeleteDevice)
	a.GET("/app-settings", s.getAppSettings)
	a.PUT("/app-settings", s.adminPutAppSettings)
	a.GET("/deletion-requests", s.adminListDeletionRequests)
	a.POST("/deletion-requests/:id/approve", s.adminApproveDeletion)
	a.POST("/deletion-requests/:id/reject", s.adminRejectDeletion)
	a.GET("/stats", s.adminStats)
	a.GET("/payments", s.adminListPayments)
	a.POST("/payments", s.adminCreatePayment)
	a.DELETE("/payments/:id", s.adminDeletePayment)
	a.GET("/announcements", s.adminListAnnouncements)
	a.POST("/announcements", s.adminCreateAnnouncement)
	a.DELETE("/announcements/:id", s.adminDeleteAnnouncement)
	a.GET("/audit-log", s.adminAuditLog)
	a.GET("/last-seen", s.adminLastSeen)

	return r
}

func (s *Server) healthz(c *gin.Context) {
	if err := s.db.Ping(c.Request.Context()); err != nil {
		slog.Error("healthz: db ping failed", "err", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_down"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
