// Package handlers wires HTTP requests to the agent loop and MongoDB store,
// and renders HTML fragments for HTMX to swap into the page — no JSON API,
// no client-side framework.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"gemini-agent-go/internal/agent"
	"gemini-agent-go/internal/auth"
	"gemini-agent-go/internal/db"
)

type ChatHandler struct {
	Agent     *agent.Agent
	Store     *db.Store
	Templates *template.Template
}

func NewChatHandler(a *agent.Agent, store *db.Store, tmpl *template.Template) *ChatHandler {
	return &ChatHandler{Agent: a, Store: store, Templates: tmpl}
}

// turnStep is the view model for one tool call, rendered as a compact
// terminal-style trace line in the chat log.
type turnStep struct {
	ToolName string
	ArgsJSON string
}

// Index renders the chat page for the logged-in user: the sidebar of all
// their conversations, plus whichever one (if any) ?c=<id> names as active.
// It's only reachable via auth.RequireAuth, so a user is always present in
// the context here.
func (h *ChatHandler) Index(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	conversations, err := h.Store.ListConversations(r.Context(), user.ID)
	if err != nil {
		log.Printf("list conversations: %v", err)
	}

	var messages []db.Message
	activeID := ""
	if idParam := r.URL.Query().Get("c"); idParam != "" {
		if id, err := primitive.ObjectIDFromHex(idParam); err == nil {
			if convo, err := h.Store.GetConversation(r.Context(), id, user.ID); err != nil {
				log.Printf("load conversation: %v", err)
			} else if convo != nil {
				messages = convo.Messages
				activeID = idParam
			}
		}
		// An invalid, missing, or not-owned id silently falls back to the
		// blank "no active chat" state rather than erroring.
	}

	if err := h.Templates.ExecuteTemplate(w, "index.html", map[string]interface{}{
		"Messages":      messages,
		"Conversations": conversations,
		"ActiveID":      activeID,
		"CurrentUser":   user,
	}); err != nil {
		log.Printf("render index: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// Chat handles a single turn: load (or create) the conversation, run the
// agent loop, persist the new messages + tool call logs, title the
// conversation if this was its first turn, and return an HTML fragment for
// HTMX to append to the chat log.
func (h *ChatHandler) Chat(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	ownerKey := user.ID.Hex() // ties tool-call audit logs to this account

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	userMsg := r.FormValue("message")
	if userMsg == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	isNew := false
	var convo *db.Conversation
	if convIDParam := r.FormValue("conversation_id"); convIDParam == "" {
		created, err := h.Store.CreateConversation(ctx, user.ID)
		if err != nil {
			log.Printf("create conversation: %v", err)
			http.Error(w, "failed to start a new chat", http.StatusInternalServerError)
			return
		}
		convo = created
		isNew = true
	} else {
		convID, err := primitive.ObjectIDFromHex(convIDParam)
		if err != nil {
			http.Error(w, "invalid conversation id", http.StatusBadRequest)
			return
		}
		found, err := h.Store.GetConversation(ctx, convID, user.ID)
		if err != nil {
			log.Printf("load conversation: %v", err)
			http.Error(w, "failed to load chat", http.StatusInternalServerError)
			return
		}
		if found == nil {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		convo = found
	}

	client, err := h.resolveClient(ctx, user)
	if err != nil {
		log.Printf("resolve AI client: %v", err)
		h.Templates.ExecuteTemplate(w, "turn.html", map[string]interface{}{
			"UserText":  userMsg,
			"ModelText": "Sorry — no AI provider is configured: " + err.Error(),
			"Steps":     []turnStep{},
		})
		return
	}

	contents := make([]agent.Content, 0, len(convo.Messages)+1)
	for _, m := range convo.Messages {
		contents = append(contents, agent.Content{Role: m.Role, Parts: []agent.Part{{Text: m.Text}}})
	}
	contents = append(contents, agent.Content{Role: "user", Parts: []agent.Part{{Text: userMsg}}})

	result, err := h.Agent.Run(ctx, contents, client)
	if err != nil {
		log.Printf("agent run: %v", err)
		h.Templates.ExecuteTemplate(w, "turn.html", map[string]interface{}{
			"UserText":  userMsg,
			"ModelText": "Sorry — something went wrong talking to the AI provider: " + err.Error(),
			"Steps":     []turnStep{},
		})
		return
	}

	now := time.Now()
	if err := h.Store.AppendMessages(ctx, convo.ID,
		db.Message{Role: "user", Text: userMsg, CreatedAt: now},
		db.Message{Role: "model", Text: result.FinalText, CreatedAt: now},
	); err != nil {
		log.Printf("save history: %v", err)
	}

	steps := make([]turnStep, 0, len(result.Steps))
	for _, s := range result.Steps {
		argsJSON, _ := json.Marshal(s.Args)
		steps = append(steps, turnStep{ToolName: s.ToolName, ArgsJSON: string(argsJSON)})

		if err := h.Store.LogToolCall(ctx, ownerKey, s.ToolName, s.Args, s.Result); err != nil {
			log.Printf("log tool call: %v", err)
		}
	}

	title := convo.Title
	if isNew {
		generated, err := agent.GenerateTitle(ctx, client, userMsg)
		if err != nil {
			log.Printf("generate title: %v", err)
			generated = truncateTitle(userMsg)
		}
		title = generated
		if err := h.Store.SetTitle(ctx, convo.ID, title); err != nil {
			log.Printf("set title: %v", err)
		}
	}

	if err := h.Templates.ExecuteTemplate(w, "turn.html", map[string]interface{}{
		"UserText":        userMsg,
		"ModelText":       result.FinalText,
		"Steps":           steps,
		"NewConversation": isNew,
		"ConversationID":  convo.ID.Hex(),
		"Title":           title,
	}); err != nil {
		log.Printf("render turn: %v", err)
	}
}

// resolveClient picks which AI provider this user's turn should use:
// - their explicitly selected config, if it's still valid (either their own
//   private config, or a shared config they're approved for) — a deleted
//   config or a revoked approval silently falls through to the default
//   below rather than erroring, since a stale selection isn't the user's
//   fault mid-conversation;
// - otherwise, the admin's app-wide active config.
// Building a client here is cheap (no network call happens until Generate),
// so resolving fresh every request is simpler and more correct than caching
// — an admin's or user's change is picked up on the very next message.
func (h *ChatHandler) resolveClient(ctx context.Context, user *db.User) (agent.LLMClient, error) {
	if user.SelectedAIConfigID != nil {
		cfg, err := h.Store.GetAIConfig(ctx, *user.SelectedAIConfigID)
		if err != nil {
			log.Printf("load selected AI config: %v", err)
		}
		if cfg != nil {
			owned := cfg.OwnerUserID != nil && *cfg.OwnerUserID == user.ID
			sharedAndApproved := cfg.Shared && user.SharedAIStatus == "approved"
			if owned || sharedAndApproved {
				return agent.NewClientForProvider(cfg.Provider, cfg.APIKey, cfg.Model, cfg.BaseURL)
			}
		}
	}

	active, err := h.Store.GetActiveAIConfig(ctx)
	if err != nil {
		return nil, err
	}
	if active == nil {
		return nil, fmt.Errorf("no AI provider configured")
	}
	return agent.NewClientForProvider(active.Provider, active.APIKey, active.Model, active.BaseURL)
}

// truncateTitle is the fallback used when title generation fails — a chat
// must always end up with some title, even if Gemini can't be reached.
func truncateTitle(msg string) string {
	const maxLen = 40
	runes := []rune(msg)
	if len(runes) <= maxLen {
		return msg
	}
	return string(runes[:maxLen]) + "…"
}

// RenameConversation lets a user retitle one of their own saved chats.
func (h *ChatHandler) RenameConversation(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid conversation id", http.StatusBadRequest)
		return
	}

	convo, err := h.Store.GetConversation(r.Context(), id, user.ID)
	if err != nil {
		log.Printf("load conversation: %v", err)
		http.Error(w, "failed to load chat", http.StatusInternalServerError)
		return
	}
	if convo == nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}

	if err := h.Store.SetTitle(r.Context(), id, title); err != nil {
		log.Printf("rename conversation: %v", err)
		http.Error(w, "failed to rename chat", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?c="+id.Hex(), http.StatusSeeOther)
}

// DeleteConversation lets a user remove one of their own saved chats.
// Ownership is enforced at the query level in Store.DeleteConversation, so
// deleting someone else's conversation id is a silent no-op, not an error.
func (h *ChatHandler) DeleteConversation(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid conversation id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	if err := h.Store.DeleteConversation(r.Context(), id, user.ID); err != nil {
		log.Printf("delete conversation: %v", err)
		http.Error(w, "failed to delete chat", http.StatusInternalServerError)
		return
	}

	// If you were viewing a different chat than the one you just deleted,
	// stay put instead of getting bounced to the blank state.
	redirectActiveID := r.FormValue("redirect_active_id")
	if redirectActiveID != "" && redirectActiveID != id.Hex() {
		http.Redirect(w, r, "/?c="+redirectActiveID, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
