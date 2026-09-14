package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/middleware"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// newAccountRequest is the wire shape of a new account, for both the operator route and
// the open sign-up one.
//
// Password is bound and never rendered. Nothing in this package puts it in a log line, a
// response or an error, which is why the two handlers below hand the whole struct
// straight to the service and keep no copy.
type newAccountRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

func (r newAccountRequest) toNewAccount() service.NewAccount {
	return service.NewAccount{
		Email:       r.Email,
		DisplayName: r.DisplayName,
		Password:    r.Password,
	}
}

// signInRequest is a credential check. The user agent and the address are taken from the
// request itself rather than the body — a client that could name its own would be naming
// what the session list shows somebody about their own devices.
type signInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// changePasswordRequest replaces a password. Both halves are required: knowing the
// current one is what makes this a password change rather than a session takeover.
type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// setActiveRequest switches an account on or off. It is a pointer so an omitted field is
// a 400 rather than a silent deactivation.
type setActiveRequest struct {
	IsActive *bool `json:"isActive"`
}

// userView is the rendered form of an account.
//
// It has no password field of any kind — not the hash, not a placeholder. The stored hash
// never leaves the repository layer, and a view struct is where that would otherwise
// quietly stop being true.
type userView struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	IsActive    bool      `json:"isActive"`
	CreatedAt   time.Time `json:"createdAt"`
}

func viewUser(user repository.User) userView {
	return userView{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		IsActive:    user.IsActive,
		CreatedAt:   user.CreatedAt,
	}
}

func viewUsers(users []repository.User) []userView {
	views := make([]userView, 0, len(users))
	for _, user := range users {
		views = append(views, viewUser(user))
	}
	return views
}

// signedInView carries the one thing that is returned exactly once.
type signedInView struct {
	// Token is the plaintext session token. Only its hash is stored, so this response
	// is the only place it exists — a client that loses it signs in again.
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      userView  `json:"user"`
}

// Register is open sign-up.
//
// The route takes no credential, which is exactly why the service checks
// CORE_OPEN_REGISTRATION itself: there is no middleware here to refuse on, so that check
// is the only one there is. A closed instance answers 403.
func (h *Handler) Register(c *gin.Context) {
	var req newAccountRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.accounts.Register(c.Request.Context(), req.toNewAccount())
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Account created.", viewUser(user))
}

// SignIn verifies a password and mints a session.
func (h *Handler) SignIn(c *gin.Context) {
	var req signInRequest
	if !bindJSON(c, &req) {
		return
	}

	signedIn, err := h.accounts.SignIn(c.Request.Context(), service.SignIn{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: c.GetHeader("User-Agent"),
		IP:        c.ClientIP(),
	})
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Signed in.", signedInView{
		Token:     signedIn.Token,
		ExpiresAt: signedIn.ExpiresAt,
		User:      viewUser(signedIn.User),
	})
}

// SignOut ends the session the caller presented.
//
// It takes the token from the request rather than an id in the path, because ending your
// own session should not require knowing its id. A machine key presented here is not a
// session, so nothing is ended and the answer is still 200: a sign-out that failed would
// leave a client unable to clear its own state.
func (h *Handler) SignOut(c *gin.Context) {
	if err := h.accounts.SignOut(c.Request.Context(), middleware.PresentedToken(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Signed out.", gin.H{"signedOut": true})
}

// Me reads the caller's own account.
func (h *Handler) Me(c *gin.Context) {
	user, err := h.accounts.Me(c.Request.Context(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Account found.", viewUser(user))
}

// ChangePassword replaces the caller's password and ends every session they have,
// including the one making this request. The client is expected to sign in again — see
// the service, where the reasoning for that lives.
func (h *Handler) ChangePassword(c *gin.Context) {
	var req changePasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	err := h.accounts.ChangePassword(c.Request.Context(), req.CurrentPassword, req.NewPassword, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Password changed. Every session has been ended, including this one.",
		gin.H{"sessionsEnded": true})
}

// CreateAccount makes an account on an operator's say-so, whether or not registration is
// open. Operator only, on the route and again in the service.
func (h *Handler) CreateAccount(c *gin.Context) {
	var req newAccountRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.accounts.Create(c.Request.Context(), req.toNewAccount(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Account created.", viewUser(user))
}

// ListAccounts reads a page of accounts. Operator only.
func (h *Handler) ListAccounts(c *gin.Context) {
	limit, offset := page(c)

	users, total, err := h.accounts.List(c.Request.Context(), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Accounts listed.", viewUsers(users), limit, offset, total)
}

// SetAccountActive switches an account on or off. Operator only.
//
// Deactivating ends every session that account holds, which is the difference between
// this and deleting the row: the person is locked out now, and what their agent did is
// still in the audit trail.
func (h *Handler) SetAccountActive(c *gin.Context) {
	var req setActiveRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.IsActive == nil {
		badRequest(c, "isActive is required, and must be true or false.")
		return
	}

	err := h.accounts.SetActive(c.Request.Context(), c.Param("id"), *req.IsActive, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}

	message := "Account deactivated. Every session it held has been ended."
	if *req.IsActive {
		message = "Account activated."
	}
	utils.Success(c, http.StatusOK, message, gin.H{"id": c.Param("id"), "isActive": *req.IsActive})
}
