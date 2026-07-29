package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"gemini-agent-go/internal/agent"
	"gemini-agent-go/internal/auth"
	"gemini-agent-go/internal/config"
	"gemini-agent-go/internal/db"
	"gemini-agent-go/internal/handlers"
)

func main() {
	cfg := config.Load()

	if cfg.MongoURI == "" {
		log.Fatal("MONGODB_URI is not set — copy .env.example to .env and add your Atlas connection string")
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := db.Connect(connectCtx, cfg.MongoURI, cfg.MongoDBName)
	if err != nil {
		log.Fatalf("mongodb connect: %v", err)
	}
	defer store.Close(context.Background())
	log.Println("connected to MongoDB Atlas")

	// ensureActiveAIConfig guarantees at least one global default config
	// exists (migrating a previously-configured key if needed) — the
	// per-request client resolution in ChatHandler.resolveClient depends on
	// GetActiveAIConfig returning something for users who haven't picked
	// their own provider.
	activeConfig, err := ensureActiveAIConfig(connectCtx, store, cfg)
	if err != nil {
		log.Fatalf("resolve active AI config: %v", err)
	}

	ag := agent.NewAgent()

	tmpl := template.Must(template.ParseGlob("templates/*.html"))

	adminEmail := os.Getenv("ADMIN_EMAIL")
	authHandler := handlers.NewAuthHandler(store, tmpl, adminEmail)
	chatHandler := handlers.NewChatHandler(ag, store, tmpl)
	adminHandler := handlers.NewAdminHandler(store, tmpl)
	settingsHandler := handlers.NewSettingsHandler(store, tmpl)

	mux := http.NewServeMux()

	// Public routes.
	mux.HandleFunc("GET /login", authHandler.LoginPage)
	mux.HandleFunc("POST /login", authHandler.Login)
	mux.HandleFunc("GET /signup", authHandler.SignupPage)
	mux.HandleFunc("POST /signup", authHandler.Signup)
	mux.HandleFunc("POST /logout", authHandler.Logout)

	// Any logged-in user.
	mux.HandleFunc("GET /{$}", auth.RequireAuth(store, chatHandler.Index))
	mux.HandleFunc("POST /chat", auth.RequireAuth(store, chatHandler.Chat))
	mux.HandleFunc("POST /conversations/{id}/rename", auth.RequireAuth(store, chatHandler.RenameConversation))
	mux.HandleFunc("POST /conversations/{id}/delete", auth.RequireAuth(store, chatHandler.DeleteConversation))

	// Any logged-in user's own AI provider settings (own configs + shared-pool access request).
	mux.HandleFunc("GET /settings", auth.RequireAuth(store, settingsHandler.Index))
	mux.HandleFunc("POST /settings/configs", auth.RequireAuth(store, settingsHandler.CreateConfig))
	mux.HandleFunc("POST /settings/configs/{id}/update", auth.RequireAuth(store, settingsHandler.UpdateConfig))
	mux.HandleFunc("POST /settings/configs/{id}/delete", auth.RequireAuth(store, settingsHandler.DeleteConfig))
	mux.HandleFunc("POST /settings/configs/{id}/test", auth.RequireAuth(store, settingsHandler.TestConfig))
	mux.HandleFunc("POST /settings/select", auth.RequireAuth(store, settingsHandler.Select))
	mux.HandleFunc("POST /settings/request-shared-access", auth.RequireAuth(store, settingsHandler.RequestSharedAccess))

	// Admin role only.
	mux.HandleFunc("GET /admin", auth.RequireAdmin(store, adminHandler.Dashboard))
	mux.HandleFunc("POST /admin/users/{id}/role", auth.RequireAdmin(store, adminHandler.UpdateRole))
	mux.HandleFunc("POST /admin/users/{id}/status", auth.RequireAdmin(store, adminHandler.UpdateStatus))
	mux.HandleFunc("POST /admin/users/{id}/shared-ai/approve", auth.RequireAdmin(store, adminHandler.ApproveSharedAccess))
	mux.HandleFunc("POST /admin/users/{id}/shared-ai/reject", auth.RequireAdmin(store, adminHandler.RejectSharedAccess))
	mux.HandleFunc("POST /admin/ai-configs", auth.RequireAdmin(store, adminHandler.CreateAIConfig))
	mux.HandleFunc("POST /admin/ai-configs/{id}/update", auth.RequireAdmin(store, adminHandler.UpdateAIConfig))
	mux.HandleFunc("POST /admin/ai-configs/{id}/delete", auth.RequireAdmin(store, adminHandler.DeleteAIConfig))
	mux.HandleFunc("POST /admin/ai-configs/{id}/activate", auth.RequireAdmin(store, adminHandler.ActivateAIConfig))
	mux.HandleFunc("POST /admin/ai-configs/{id}/test", auth.RequireAdmin(store, adminHandler.TestAIConfig))
	mux.HandleFunc("POST /admin/ai-configs/{id}/shared", auth.RequireAdmin(store, adminHandler.SetConfigShared))

	// Cache-Control: no-cache (not no-store) so the browser still revalidates
	// with a conditional request instead of caching style.css blindly for its
	// default lifetime — otherwise CSS edits during development silently
	// don't show up until a hard refresh.
	staticFiles := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticFiles.ServeHTTP(w, r)
	}))

	addr := ":" + cfg.Port
	log.Printf("listening on http://localhost%s (provider: %s, model: %s)", addr, activeConfig.Provider, activeConfig.Model)
	if adminEmail == "" {
		log.Println("note: ADMIN_EMAIL is not set — no signup will get the admin role until you set it")
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// ensureActiveAIConfig resolves which AIConfig should be active at startup:
// the DB's existing active config if one's already saved, otherwise a
// one-time migration seeding a "Default" Gemini config from either the
// previous single-config Settings feature (GetGeminiSettings) or .env —
// whichever has a key — so nothing already configured is lost.
func ensureActiveAIConfig(ctx context.Context, store *db.Store, cfg *config.Config) (*db.AIConfig, error) {
	if active, err := store.GetActiveAIConfig(ctx); err != nil {
		return nil, fmt.Errorf("load active AI config: %w", err)
	} else if active != nil {
		return active, nil
	}

	apiKey, model := cfg.GeminiAPIKey, cfg.GeminiModel
	if settings, err := store.GetGeminiSettings(ctx); err != nil {
		log.Printf("load legacy gemini settings: %v", err)
	} else if settings != nil && settings.APIKey != "" {
		apiKey, model = settings.APIKey, settings.Model
		log.Println("migrating previous admin settings into a new AI config")
	}

	if apiKey == "" {
		return nil, fmt.Errorf("no AI config is active and GEMINI_API_KEY is not set — copy .env.example to .env and fill it in, or add a config from /admin once running")
	}

	created, err := store.CreateAIConfig(ctx, db.AIConfig{
		Name:     "Default",
		Provider: "gemini",
		APIKey:   apiKey,
		Model:    model,
		IsActive: true,
	})
	if err != nil {
		return nil, fmt.Errorf("seed default AI config: %w", err)
	}
	return created, nil
}
