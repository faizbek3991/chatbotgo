package handlers

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"gemini-agent-go/internal/agent"
	"gemini-agent-go/internal/auth"
	"gemini-agent-go/internal/db"
)

type AdminHandler struct {
	Store     *db.Store
	Templates *template.Template
}

func NewAdminHandler(store *db.Store, tmpl *template.Template) *AdminHandler {
	return &AdminHandler{Store: store, Templates: tmpl}
}

// Dashboard is only reachable via auth.RequireAdmin, so a user is always
// present in the context by the time this runs.
func (h *AdminHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	currentUser, _ := auth.UserFromContext(r.Context())

	users, err := h.Store.ListUsers(r.Context())
	if err != nil {
		log.Printf("list users: %v", err)
	}

	recentCalls, err := h.Store.RecentToolCalls(r.Context(), 25)
	if err != nil {
		log.Printf("recent tool calls: %v", err)
	}

	aiConfigs, err := h.Store.ListAIConfigs(r.Context())
	if err != nil {
		log.Printf("list ai configs: %v", err)
	}

	if err := h.Templates.ExecuteTemplate(w, "admin.html", map[string]interface{}{
		"CurrentUser": currentUser,
		"Users":       users,
		"ToolCalls":   recentCalls,
		"AIConfigs":   aiConfigs,
		"ActiveTab":   r.URL.Query().Get("tab"),
		"Tested":      r.URL.Query().Get("tested"),
		"TestError":   r.URL.Query().Get("test_error"),
	}); err != nil {
		log.Printf("render admin: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// UpdateRole promotes/demotes the target user between "user" and "admin".
// An admin can't change their own role — that's a self-lockout footgun (the
// only path back would be editing Atlas directly).
func (h *AdminHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	currentUser, _ := auth.UserFromContext(r.Context())

	targetID, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if targetID == currentUser.ID {
		http.Error(w, "you can't change your own role", http.StatusBadRequest)
		return
	}

	role := r.FormValue("role")
	if role != "user" && role != "admin" {
		http.Error(w, "invalid role", http.StatusBadRequest)
		return
	}

	if err := h.Store.UpdateUserRole(r.Context(), targetID, role); err != nil {
		log.Printf("update user role: %v", err)
		http.Error(w, "failed to update role", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// UpdateStatus blocks or reactivates the target user. Blocking takes effect
// immediately — see auth.RequireAuth, which re-checks Status on every
// request rather than only at login. An admin can't block themselves.
func (h *AdminHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	currentUser, _ := auth.UserFromContext(r.Context())

	targetID, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if targetID == currentUser.ID {
		http.Error(w, "you can't block your own account", http.StatusBadRequest)
		return
	}

	status := r.FormValue("status")
	if status != "active" && status != "blocked" {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	if err := h.Store.SetUserStatus(r.Context(), targetID, status); err != nil {
		log.Printf("update user status: %v", err)
		http.Error(w, "failed to update status", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

var validProviders = map[string]bool{"gemini": true, "openai": true, "anthropic": true}

// CreateAIConfig saves a new named AI provider configuration.
func (h *AdminHandler) CreateAIConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	provider := r.FormValue("provider")
	model := r.FormValue("model")
	apiKey := r.FormValue("api_key")
	baseURL := r.FormValue("base_url")

	if name == "" || model == "" {
		http.Error(w, "name and model are required", http.StatusBadRequest)
		return
	}
	if !validProviders[provider] {
		http.Error(w, "invalid provider", http.StatusBadRequest)
		return
	}

	if _, err := h.Store.CreateAIConfig(r.Context(), db.AIConfig{
		Name:     name,
		Provider: provider,
		APIKey:   apiKey,
		Model:    model,
		BaseURL:  baseURL,
	}); err != nil {
		log.Printf("create ai config: %v", err)
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?tab=settings", http.StatusSeeOther)
}

// UpdateAIConfig edits a saved config. A blank api_key field means "keep
// the existing key" — the real key never round-trips through the edit form.
func (h *AdminHandler) UpdateAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	model := r.FormValue("model")
	baseURL := r.FormValue("base_url")
	if name == "" || model == "" {
		http.Error(w, "name and model are required", http.StatusBadRequest)
		return
	}

	var apiKey *string
	if v := r.FormValue("api_key"); v != "" {
		apiKey = &v
	}

	if err := h.Store.UpdateAIConfig(r.Context(), id, name, model, baseURL, apiKey); err != nil {
		log.Printf("update ai config: %v", err)
		http.Error(w, "failed to update config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?tab=settings", http.StatusSeeOther)
}

// DeleteAIConfig removes a saved config. Refuses to delete the active one —
// activate a different config first, same self-lockout protection as the
// user-role/status guards above.
func (h *AdminHandler) DeleteAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}

	cfg, err := h.Store.GetAIConfig(r.Context(), id)
	if err != nil {
		log.Printf("load ai config: %v", err)
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}
	if cfg.IsActive {
		http.Error(w, "can't delete the active config — activate a different one first", http.StatusBadRequest)
		return
	}

	if err := h.Store.DeleteAIConfig(r.Context(), id); err != nil {
		log.Printf("delete ai config: %v", err)
		http.Error(w, "failed to delete config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?tab=settings", http.StatusSeeOther)
}

// ActivateAIConfig switches the app-wide default provider immediately — no
// restart needed, and no in-memory state to update either, since every
// chat turn resolves its client fresh from the DB (see
// ChatHandler.resolveClient) — this is just a DB flag flip.
func (h *AdminHandler) ActivateAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}

	cfg, err := h.Store.GetAIConfig(r.Context(), id)
	if err != nil {
		log.Printf("load ai config: %v", err)
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	if err := h.Store.SetActiveAIConfig(r.Context(), id); err != nil {
		log.Printf("set active ai config: %v", err)
		http.Error(w, "failed to activate config", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/admin?tab=settings", http.StatusSeeOther)
}

// SetConfigShared toggles whether a global config is available to approved
// users' shared AI pool.
func (h *AdminHandler) SetConfigShared(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	shared := r.FormValue("shared") == "true"

	if err := h.Store.SetConfigShared(r.Context(), id, shared); err != nil {
		log.Printf("set config shared: %v", err)
		http.Error(w, "failed to update config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?tab=settings", http.StatusSeeOther)
}

// ApproveSharedAccess grants a user access to pick from the shared AI pool.
func (h *AdminHandler) ApproveSharedAccess(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if err := h.Store.SetSharedAIStatus(r.Context(), id, "approved"); err != nil {
		log.Printf("approve shared ai access: %v", err)
		http.Error(w, "failed to approve", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// RejectSharedAccess denies a user's request to use the shared AI pool.
func (h *AdminHandler) RejectSharedAccess(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if err := h.Store.SetSharedAIStatus(r.Context(), id, "rejected"); err != nil {
		log.Printf("reject shared ai access: %v", err)
		http.Error(w, "failed to reject", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// TestAIConfig sends one minimal real request through a throwaway client
// built from the saved config — never the shared active one — and reports
// success or failure via a query-param flash on the redirect back to /admin.
func (h *AdminHandler) TestAIConfig(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}

	cfg, err := h.Store.GetAIConfig(r.Context(), id)
	if err != nil {
		log.Printf("load ai config: %v", err)
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	client, err := agent.NewClientForProvider(cfg.Provider, cfg.APIKey, cfg.Model, cfg.BaseURL)
	if err != nil {
		http.Error(w, "failed to build client: "+err.Error(), http.StatusBadRequest)
		return
	}

	testCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	idHex := id.Hex()
	if err := agent.TestConnection(testCtx, client); err != nil {
		http.Redirect(w, r, "/admin?tab=settings&tested="+idHex+"&test_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin?tab=settings&tested="+idHex, http.StatusSeeOther)
}
