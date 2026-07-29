package handlers

import (
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"gemini-agent-go/internal/auth"
	"gemini-agent-go/internal/db"
)

// sessionDuration is how long a login lasts before needing to sign in again.
const sessionDuration = 7 * 24 * time.Hour

type AuthHandler struct {
	Store      *db.Store
	Templates  *template.Template
	AdminEmail string // optional: signups matching this email become admin
}

func NewAuthHandler(store *db.Store, tmpl *template.Template, adminEmail string) *AuthHandler {
	return &AuthHandler{Store: store, Templates: tmpl, AdminEmail: adminEmail}
}

func (h *AuthHandler) SignupPage(w http.ResponseWriter, r *http.Request) {
	h.Templates.ExecuteTemplate(w, "signup.html", nil)
}

func (h *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm_password")

	switch {
	case email == "" || password == "":
		h.signupError(w, "Email and password are required.")
		return
	case len(password) < 8:
		h.signupError(w, "Password must be at least 8 characters.")
		return
	case password != confirm:
		h.signupError(w, "Passwords don't match.")
		return
	}

	ctx := r.Context()

	if existing, err := h.Store.GetUserByEmail(ctx, email); err != nil {
		log.Printf("check existing user: %v", err)
		h.signupError(w, "Something went wrong. Try again.")
		return
	} else if existing != nil {
		h.signupError(w, "An account with that email already exists.")
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Printf("hash password: %v", err)
		h.signupError(w, "Something went wrong. Try again.")
		return
	}

	role := "user"
	if h.AdminEmail != "" && strings.EqualFold(h.AdminEmail, email) {
		role = "admin"
	}

	user, err := h.Store.CreateUser(ctx, email, hash, role)
	if err != nil {
		log.Printf("create user: %v", err)
		h.signupError(w, "Something went wrong. Try again.")
		return
	}

	h.startSession(w, r, user)
}

func (h *AuthHandler) signupError(w http.ResponseWriter, msg string) {
	h.Templates.ExecuteTemplate(w, "signup.html", map[string]interface{}{"Error": msg})
}

func (h *AuthHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.Templates.ExecuteTemplate(w, "login.html", nil)
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")

	user, err := h.Store.GetUserByEmail(r.Context(), email)
	if err != nil {
		log.Printf("get user by email: %v", err)
		h.loginError(w, "Something went wrong. Try again.")
		return
	}
	if user == nil || !auth.CheckPassword(user.PasswordHash, password) {
		h.loginError(w, "Incorrect email or password.")
		return
	}
	if user.Status == "blocked" {
		h.loginError(w, "Your account has been blocked.")
		return
	}

	h.startSession(w, r, user)
}

func (h *AuthHandler) loginError(w http.ResponseWriter, msg string) {
	h.Templates.ExecuteTemplate(w, "login.html", map[string]interface{}{"Error": msg})
}

// startSession creates a new server-side session, sets the cookie, and
// redirects to the chat page — the shared last step of signup and login.
func (h *AuthHandler) startSession(w http.ResponseWriter, r *http.Request, user *db.User) {
	token, err := auth.NewToken()
	if err != nil {
		log.Printf("generate token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(sessionDuration)
	if err := h.Store.CreateSession(r.Context(), token, user.ID, expiresAt); err != nil {
		log.Printf("create session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Expires:  expiresAt,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout deletes the session server-side (not just the cookie), so the
// token can't be replayed even if it leaked.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if err := h.Store.DeleteSession(r.Context(), cookie.Value); err != nil {
			log.Printf("delete session: %v", err)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
