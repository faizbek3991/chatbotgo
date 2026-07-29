// Package handlers: SettingsHandler is the user-facing counterpart to the
// admin AI Providers tab — each user can save their own private provider
// configs (their own API keys), request approval to use the admin's shared
// pool, and choose which config their own chats should use.
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

type SettingsHandler struct {
	Store     *db.Store
	Templates *template.Template
}

func NewSettingsHandler(store *db.Store, tmpl *template.Template) *SettingsHandler {
	return &SettingsHandler{Store: store, Templates: tmpl}
}

// Index shows the user's own configs, the shared pool (if approved, or a
// request-access prompt otherwise), and which config is currently selected.
func (h *SettingsHandler) Index(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	ownConfigs, err := h.Store.ListUserAIConfigs(r.Context(), user.ID)
	if err != nil {
		log.Printf("list user ai configs: %v", err)
	}

	var sharedConfigs []db.AIConfig
	if user.SharedAIStatus == "approved" {
		sharedConfigs, err = h.Store.ListSharedAIConfigs(r.Context())
		if err != nil {
			log.Printf("list shared ai configs: %v", err)
		}
	}

	selectedID := ""
	if user.SelectedAIConfigID != nil {
		selectedID = user.SelectedAIConfigID.Hex()
	}

	if err := h.Templates.ExecuteTemplate(w, "settings.html", map[string]interface{}{
		"CurrentUser":    user,
		"OwnConfigs":     ownConfigs,
		"SharedConfigs":  sharedConfigs,
		"SharedAIStatus": user.SharedAIStatus,
		"SelectedID":     selectedID,
		"Tested":         r.URL.Query().Get("tested"),
		"TestError":      r.URL.Query().Get("test_error"),
	}); err != nil {
		log.Printf("render settings: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// CreateConfig saves a new private config, owned by the current user.
func (h *SettingsHandler) CreateConfig(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

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
		Name:        name,
		Provider:    provider,
		APIKey:      apiKey,
		Model:       model,
		BaseURL:     baseURL,
		OwnerUserID: &user.ID,
	}); err != nil {
		log.Printf("create user ai config: %v", err)
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// ownedConfig loads a config and verifies it belongs to the requesting
// user — the same "not found or not yours" convention used elsewhere
// (GetConversation), returning nil rather than a 403 for a not-owned id so
// callers can't distinguish "doesn't exist" from "isn't yours".
func (h *SettingsHandler) ownedConfig(ctx context.Context, id, userID primitive.ObjectID) (*db.AIConfig, error) {
	cfg, err := h.Store.GetAIConfig(ctx, id)
	if err != nil || cfg == nil {
		return nil, err
	}
	if cfg.OwnerUserID == nil || *cfg.OwnerUserID != userID {
		return nil, nil
	}
	return cfg, nil
}

// UpdateConfig edits one of the user's own configs. A blank api_key field
// means "keep the existing key" — same convention as the admin tab.
func (h *SettingsHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	cfg, err := h.ownedConfig(r.Context(), id, user.ID)
	if err != nil {
		log.Printf("load user ai config: %v", err)
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "config not found", http.StatusNotFound)
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
		log.Printf("update user ai config: %v", err)
		http.Error(w, "failed to update config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// DeleteConfig removes one of the user's own configs. If it was their
// current selection, that selection is cleared too — falling back to the
// app default rather than leaving a dangling reference.
func (h *SettingsHandler) DeleteConfig(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	cfg, err := h.ownedConfig(r.Context(), id, user.ID)
	if err != nil {
		log.Printf("load user ai config: %v", err)
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	if err := h.Store.DeleteAIConfig(r.Context(), id); err != nil {
		log.Printf("delete user ai config: %v", err)
		http.Error(w, "failed to delete config", http.StatusInternalServerError)
		return
	}
	if user.SelectedAIConfigID != nil && *user.SelectedAIConfigID == id {
		if err := h.Store.SetSelectedAIConfig(r.Context(), user.ID, nil); err != nil {
			log.Printf("clear selected ai config: %v", err)
		}
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// TestConfig sends one minimal real request through a throwaway client
// built from the user's own config, reporting success/failure the same way
// the admin tab's Test action does.
func (h *SettingsHandler) TestConfig(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	cfg, err := h.ownedConfig(r.Context(), id, user.ID)
	if err != nil {
		log.Printf("load user ai config: %v", err)
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
		http.Redirect(w, r, "/settings?tested="+idHex+"&test_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?tested="+idHex, http.StatusSeeOther)
}

// Select sets which config the user's own chats should use. An empty
// config_id reverts to the app-wide default. Ownership/shared+approved is
// validated here too — the same check resolveClient does per chat turn —
// so a stale selection can never be saved in the first place.
func (h *SettingsHandler) Select(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	idParam := r.FormValue("config_id")
	if idParam == "" {
		if err := h.Store.SetSelectedAIConfig(r.Context(), user.ID, nil); err != nil {
			log.Printf("clear selected ai config: %v", err)
			http.Error(w, "failed to update selection", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	id, err := primitive.ObjectIDFromHex(idParam)
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
	owned := cfg != nil && cfg.OwnerUserID != nil && *cfg.OwnerUserID == user.ID
	sharedAndApproved := cfg != nil && cfg.Shared && user.SharedAIStatus == "approved"
	if cfg == nil || !(owned || sharedAndApproved) {
		http.Error(w, "config not found or not available to you", http.StatusNotFound)
		return
	}

	if err := h.Store.SetSelectedAIConfig(r.Context(), user.ID, &id); err != nil {
		log.Printf("set selected ai config: %v", err)
		http.Error(w, "failed to update selection", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// RequestSharedAccess marks the user's request to use the shared/paid AI
// pool as pending admin review.
func (h *SettingsHandler) RequestSharedAccess(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	if err := h.Store.RequestSharedAIAccess(r.Context(), user.ID); err != nil {
		log.Printf("request shared ai access: %v", err)
		http.Error(w, "failed to submit request", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
