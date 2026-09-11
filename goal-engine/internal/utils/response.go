// Package utils holds the shared HTTP response envelope and error codes.
//
// The envelope matches the convention used across this portfolio: camelCase
// JSON, a success flag, and request-scoped metadata on every response.
package utils

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ContextKeyRequestID is where the request-id middleware stores its value.
const ContextKeyRequestID = "request_id"

// location is the timezone used for rendered timestamps. It defaults to UTC and
// is replaced at startup by SetLocation.
var location = time.UTC

// SetLocation sets the timezone used by NowISO. Call it once during startup.
func SetLocation(loc *time.Location) {
	if loc != nil {
		location = loc
	}
}

// Response is the standard API response envelope.
type Response struct {
	Success bool        `json:"success"`
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	Error   *ErrorInfo  `json:"error,omitempty"`
	Meta    Meta        `json:"meta"`
}

// ErrorInfo provides details for error responses.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Fields carries per-field validation messages when present.
	Fields map[string]string `json:"fields,omitempty"`
}

// Meta contains request-scoped metadata.
type Meta struct {
	RequestID  string      `json:"requestId"`
	Timestamp  string      `json:"timestamp"`
	Pagination *Pagination `json:"pagination,omitempty"`
}

// Pagination holds pagination metadata for list responses.
type Pagination struct {
	Page       int `json:"page"`
	Limit      int `json:"limit"`
	TotalItems int `json:"totalItems"`
	TotalPages int `json:"totalPages"`
}

// Success writes a success response with the standard envelope.
func Success(c *gin.Context, code int, message string, data interface{}) {
	c.JSON(code, Response{
		Success: true,
		Code:    code,
		Message: message,
		Data:    data,
		Meta:    newMeta(c, nil),
	})
}

// SuccessWithPagination writes a success response with pagination metadata.
func SuccessWithPagination(c *gin.Context, code int, message string, data interface{}, page, limit, totalItems int) {
	if page <= 0 {
		page = 1
	}
	if limit <= 0 {
		limit = 50
	}
	totalPages := 0
	if limit > 0 {
		totalPages = (totalItems + limit - 1) / limit
	}
	c.JSON(code, Response{
		Success: true,
		Code:    code,
		Message: message,
		Data:    data,
		Meta: newMeta(c, &Pagination{
			Page:       page,
			Limit:      limit,
			TotalItems: totalItems,
			TotalPages: totalPages,
		}),
	})
}

// Error writes an error response with the provided API error code and message.
func Error(c *gin.Context, code int, errCode, message string) {
	c.JSON(code, Response{
		Success: false,
		Code:    code,
		Message: "Failed",
		Error:   &ErrorInfo{Code: errCode, Message: message},
		Meta:    newMeta(c, nil),
	})
}

// ValidationError writes an error response carrying per-field messages.
func ValidationError(c *gin.Context, message string, fields map[string]string) {
	c.JSON(400, Response{
		Success: false,
		Code:    400,
		Message: "Failed",
		Error:   &ErrorInfo{Code: ErrCodeValidation, Message: message, Fields: fields},
		Meta:    newMeta(c, nil),
	})
}

func newMeta(c *gin.Context, p *Pagination) Meta {
	return Meta{
		RequestID:  RequestID(c),
		Timestamp:  NowISO(),
		Pagination: p,
	}
}

// RequestID returns the current request's id, generating one if the middleware
// did not run.
func RequestID(c *gin.Context) string {
	if c == nil {
		return uuid.New().String()
	}
	if id := c.GetString(ContextKeyRequestID); id != "" {
		return id
	}
	return uuid.New().String()
}

// NowISO returns the current time in ISO 8601 format in the configured zone.
func NowISO() string {
	return time.Now().In(location).Format(time.RFC3339)
}
