package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// APIError is rendered as {"error":{"code","message"}} (HANDOFF 2).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func newErr(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

var (
	errUnauthorized       = newErr(http.StatusUnauthorized, "UNAUTHORIZED", "token yaroqsiz yoki eskirgan")
	errInvalidCredentials = newErr(http.StatusUnauthorized, "INVALID_CREDENTIALS", "login yoki parol noto'g'ri")
	errNotFound           = newErr(http.StatusNotFound, "NOT_FOUND", "topilmadi")
	errForbiddenNotPro    = newErr(http.StatusForbidden, "FORBIDDEN_NOT_PRO", "bulutga yozish uchun faol PRO tarif kerak")
	errAccountBlocked     = newErr(http.StatusForbidden, "ACCOUNT_BLOCKED", "akkaunt bloklangan")
	errForbidden          = newErr(http.StatusForbidden, "FORBIDDEN", "ruxsat yo'q")
	errDeviceLimit        = newErr(http.StatusConflict, "DEVICE_LIMIT_REACHED", "qurilmalar limiti to'lgan")
	errLoginTaken         = newErr(http.StatusConflict, "LOGIN_TAKEN", "bu login band")
	errInvalidLogin       = newErr(http.StatusBadRequest, "INVALID_LOGIN", "login 3-32 ta kichik lotin harf, raqam yoki _ bo'lishi kerak")
	errWeakPassword       = newErr(http.StatusBadRequest, "WEAK_PASSWORD", "parol kamida 6 belgi bo'lishi kerak")
	errRateLimited        = newErr(http.StatusTooManyRequests, "RATE_LIMITED", "urinishlar juda ko'p, keyinroq qayta urining")
	errRequestExists      = newErr(http.StatusConflict, "REQUEST_EXISTS", "bu akkaunt uchun so'rov allaqachon yuborilgan")
	errInternal           = newErr(http.StatusInternalServerError, "INTERNAL", "server xatosi")
)

func validation(msg string) *APIError {
	return newErr(http.StatusUnprocessableEntity, "VALIDATION", msg)
}

// fail renders err and aborts. Unknown errors become 500 INTERNAL (logged, not exposed).
func fail(c *gin.Context, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		slog.ErrorContext(c.Request.Context(), "internal error",
			"err", err, "path", c.FullPath(), "request_id", c.GetString(ctxRequestID))
		ae = errInternal
	}
	c.AbortWithStatusJSON(ae.Status, gin.H{"error": gin.H{"code": ae.Code, "message": ae.Message}})
}
