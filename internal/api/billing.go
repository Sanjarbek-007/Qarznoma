package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// pgTime is used for nullable timestamptz scans.
type pgTime = time.Time

// Google Play Billing (HANDOFF 8) — phase 2. Until the Play Developer API service account and
// Pub/Sub topic exist these return 501 so the app (PlayBillingConfig.ENABLED=false) can't charge
// users without granting the plan.
var errBillingNotConfigured = newErr(http.StatusNotImplemented, "NOT_CONFIGURED", "Google Play Billing hali sozlanmagan")

func (s *Server) billingVerify(c *gin.Context) { fail(c, errBillingNotConfigured) }
func (s *Server) billingRTDN(c *gin.Context)   { fail(c, errBillingNotConfigured) }
