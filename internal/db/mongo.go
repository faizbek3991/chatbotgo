// Package db wraps the MongoDB Atlas collections used by the agent:
// conversation history (so the chat survives a refresh) and a tool-call
// audit log (so you can see exactly what the agent did and when).
package db

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Message mirrors one turn in the Gemini conversation. Role is "user" or
// "model", matching the roles Gemini's API expects.
type Message struct {
	Role      string    `bson:"role" json:"role"`
	Text      string    `bson:"text" json:"text"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// Conversation is one saved chat thread belonging to a user. A user can
// have many of these — each gets its own auto-generated Title, shown in
// the sidebar.
type Conversation struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	UserID    primitive.ObjectID `bson:"user_id"`
	Title     string             `bson:"title"`
	Messages  []Message          `bson:"messages"`
	CreatedAt time.Time          `bson:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at"`
}

// ToolCallLog records a single tool invocation for the audit trail.
type ToolCallLog struct {
	SessionID string                 `bson:"session_id"`
	ToolName  string                 `bson:"tool_name"`
	Args      map[string]interface{} `bson:"args"`
	Result    map[string]interface{} `bson:"result"`
	CreatedAt time.Time              `bson:"created_at"`
}

// User is an account. Role is "user" (default) or "admin". Status is
// "active" or "blocked" — accounts created before this field existed decode
// with an empty Status, which every check below treats as active.
//
// SharedAIStatus tracks a user's request to use the admin's shared/paid AI
// pool: "" (never asked), "pending", "approved", or "rejected" — approval is
// a manual admin action, not real payment processing (see AIConfig.Shared).
// SelectedAIConfigID is which AIConfig this user wants their chats to use;
// nil means "use whatever the admin has marked as the global active config"
// — the original, single-shared-provider behavior, unchanged for anyone who
// never visits /settings.
type User struct {
	ID                 primitive.ObjectID  `bson:"_id,omitempty"`
	Email              string              `bson:"email"`
	PasswordHash       string              `bson:"password_hash"`
	Role               string              `bson:"role"`
	Status             string              `bson:"status"`
	SharedAIStatus     string              `bson:"shared_ai_status"`
	SelectedAIConfigID *primitive.ObjectID `bson:"selected_ai_config_id,omitempty"`
	CreatedAt          time.Time           `bson:"created_at"`
}

// Session is a server-side login session. The cookie only ever holds the
// opaque Token — looking it up here is what proves who's asking.
type Session struct {
	Token     string             `bson:"token"`
	UserID    primitive.ObjectID `bson:"user_id"`
	ExpiresAt time.Time          `bson:"expires_at"`
	CreatedAt time.Time          `bson:"created_at"`
}

// GeminiSettings holds the admin-editable API key + model, stored as a
// single document keyed by a fixed ID so it's always upserted in place.
type GeminiSettings struct {
	ID     string `bson:"_id"`
	APIKey string `bson:"api_key"`
	Model  string `bson:"model"`
}

const geminiSettingsID = "gemini"

type Store struct {
	client    *mongo.Client
	convos    *mongo.Collection
	toolLogs  *mongo.Collection
	users     *mongo.Collection
	sessions  *mongo.Collection
	settings  *mongo.Collection
	aiConfigs *mongo.Collection
}

// Connect opens (and pings) a MongoDB Atlas connection, ensures the indexes
// this app depends on exist, and returns a Store wrapping its collections.
func Connect(ctx context.Context, uri, dbName string) (*Store, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		return nil, err
	}

	database := client.Database(dbName)
	store := &Store{
		client:    client,
		convos:    database.Collection("conversations"),
		toolLogs:  database.Collection("tool_calls"),
		users:     database.Collection("users"),
		sessions:  database.Collection("sessions"),
		settings:  database.Collection("settings"),
		aiConfigs: database.Collection("ai_configs"),
	}

	// One email per account.
	if _, err := store.users.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return nil, fmt.Errorf("create users email index: %w", err)
	}

	// TTL index: MongoDB itself deletes session documents once expires_at
	// is in the past — no manual cleanup job needed.
	if _, err := store.sessions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}); err != nil {
		return nil, fmt.Errorf("create sessions ttl index: %w", err)
	}

	// Speeds up "list this user's conversations, newest first" for the sidebar.
	if _, err := store.convos.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "updated_at", Value: -1}},
	}); err != nil {
		return nil, fmt.Errorf("create conversations user_id/updated_at index: %w", err)
	}

	return store, nil
}

func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// CreateConversation starts a new, empty chat thread for a user. Its title
// is a placeholder until the first turn is titled via SetTitle.
func (s *Store) CreateConversation(ctx context.Context, userID primitive.ObjectID) (*Conversation, error) {
	now := time.Now()
	convo := Conversation{
		ID:        primitive.NewObjectID(),
		UserID:    userID,
		Title:     "New chat",
		Messages:  []Message{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := s.convos.InsertOne(ctx, convo); err != nil {
		return nil, err
	}
	return &convo, nil
}

// GetConversation loads a conversation, scoped to the given owner. Filtering
// on userID here (not just after the fact) is what stops one user from
// loading another's chat by guessing an ID. Returns (nil, nil) — not an
// error — when there's no match, since "not found or not yours" is an
// expected, non-exceptional outcome for callers to fall back on.
func (s *Store) GetConversation(ctx context.Context, id, userID primitive.ObjectID) (*Conversation, error) {
	var convo Conversation
	err := s.convos.FindOne(ctx, bson.M{"_id": id, "user_id": userID}).Decode(&convo)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &convo, nil
}

// ListConversations returns a user's conversations, newest-first, for the
// sidebar. Messages are excluded from the projection — the sidebar only
// needs titles and timestamps, not full history.
func (s *Store) ListConversations(ctx context.Context, userID primitive.ObjectID) ([]Conversation, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetProjection(bson.M{"title": 1, "updated_at": 1})

	cursor, err := s.convos.Find(ctx, bson.M{"user_id": userID}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	convos := []Conversation{}
	if err := cursor.All(ctx, &convos); err != nil {
		return nil, err
	}
	return convos, nil
}

// DeleteConversation removes a conversation, scoped to the given owner —
// same ownership-at-the-query-level pattern as GetConversation, so a user
// can't delete another user's conversation by guessing an ID.
func (s *Store) DeleteConversation(ctx context.Context, id, userID primitive.ObjectID) error {
	_, err := s.convos.DeleteOne(ctx, bson.M{"_id": id, "user_id": userID})
	return err
}

// AppendMessages pushes new messages onto a conversation's history.
func (s *Store) AppendMessages(ctx context.Context, conversationID primitive.ObjectID, msgs ...Message) error {
	_, err := s.convos.UpdateOne(
		ctx,
		bson.M{"_id": conversationID},
		bson.M{
			"$push": bson.M{"messages": bson.M{"$each": msgs}},
			"$set":  bson.M{"updated_at": time.Now()},
		},
	)
	return err
}

// SetTitle sets a conversation's auto-generated title, once its first turn
// has produced one.
func (s *Store) SetTitle(ctx context.Context, conversationID primitive.ObjectID, title string) error {
	_, err := s.convos.UpdateOne(
		ctx,
		bson.M{"_id": conversationID},
		bson.M{"$set": bson.M{"title": title}},
	)
	return err
}

// LogToolCall records one tool invocation (name, args, result) for the
// audit trail — useful for a "show your work" panel or debugging in class.
func (s *Store) LogToolCall(ctx context.Context, sessionID, toolName string, args, result map[string]interface{}) error {
	_, err := s.toolLogs.InsertOne(ctx, ToolCallLog{
		SessionID: sessionID,
		ToolName:  toolName,
		Args:      args,
		Result:    result,
		CreatedAt: time.Now(),
	})
	return err
}

// RecentToolCalls returns the most recent tool-call log entries across all
// users, newest first — used by the admin dashboard.
func (s *Store) RecentToolCalls(ctx context.Context, limit int64) ([]ToolCallLog, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(limit)

	cursor, err := s.toolLogs.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var calls []ToolCallLog
	if err := cursor.All(ctx, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// --- Users ---

// CreateUser inserts a new account. Returns a Mongo duplicate-key error if
// the email is already registered (enforced by the unique index in Connect).
func (s *Store) CreateUser(ctx context.Context, email, passwordHash, role string) (*User, error) {
	user := User{
		ID:           primitive.NewObjectID(),
		Email:        email,
		PasswordHash: passwordHash,
		Role:         role,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if _, err := s.users.InsertOne(ctx, user); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUserByEmail returns (nil, nil) — not an error — when there's no match,
// since "no such user" is an expected outcome during login/signup checks.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var user User
	err := s.users.FindOne(ctx, bson.M{"email": email}).Decode(&user)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// HasAdmin reports whether any account already holds the admin role. Signup
// uses this so ADMIN_EMAIL auto-promotion only ever applies to the very
// first account claiming it — once an admin exists, a leaked or guessed
// ADMIN_EMAIL address can no longer be used to self-promote by signing up
// with it.
func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	count, err := s.users.CountDocuments(ctx, bson.M{"role": "admin"})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) GetUserByID(ctx context.Context, id primitive.ObjectID) (*User, error) {
	var user User
	err := s.users.FindOne(ctx, bson.M{"_id": id}).Decode(&user)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// UpdateUserRole promotes/demotes an account between "user" and "admin".
func (s *Store) UpdateUserRole(ctx context.Context, userID primitive.ObjectID, role string) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": bson.M{"role": role}})
	return err
}

// SetUserStatus blocks or reactivates an account. Blocking takes effect
// immediately on the account's very next request — see auth.RequireAuth,
// which re-checks Status on every request rather than only at login.
func (s *Store) SetUserStatus(ctx context.Context, userID primitive.ObjectID, status string) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": bson.M{"status": status}})
	return err
}

// ListUsers returns every account — used by the admin dashboard.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	cursor, err := s.users.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// --- Sessions ---

func (s *Store) CreateSession(ctx context.Context, token string, userID primitive.ObjectID, expiresAt time.Time) error {
	_, err := s.sessions.InsertOne(ctx, Session{
		Token:     token,
		UserID:    userID,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	})
	return err
}

func (s *Store) GetSession(ctx context.Context, token string) (*Session, error) {
	var session Session
	if err := s.sessions.FindOne(ctx, bson.M{"token": token}).Decode(&session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.sessions.DeleteOne(ctx, bson.M{"token": token})
	return err
}

// --- Settings ---

// GetGeminiSettings returns the previous single-config admin settings, or
// (nil, nil) if never set. Only used once, at startup, to migrate an
// existing key into the new multi-provider ai_configs collection — see
// AIConfig below. Superseded otherwise; there's no SetGeminiSettings
// anymore, since AIConfig replaces it going forward.
func (s *Store) GetGeminiSettings(ctx context.Context) (*GeminiSettings, error) {
	var settings GeminiSettings
	err := s.settings.FindOne(ctx, bson.M{"_id": geminiSettingsID}).Decode(&settings)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

// --- AI configs ---

// AIConfig is one saved, named AI provider configuration. A global config
// (OwnerUserID nil) is admin-managed; exactly one of those is ever
// IsActive — the app-wide default for any user who hasn't picked their own.
// A global config can also be marked Shared, making it selectable by users
// whose SharedAIStatus is "approved". A user-owned config (OwnerUserID set)
// is private to that user and never shared; IsActive/Shared are meaningless
// on it.
type AIConfig struct {
	ID          primitive.ObjectID  `bson:"_id,omitempty"`
	Name        string              `bson:"name"`
	Provider    string              `bson:"provider"` // "gemini" | "openai" | "anthropic"
	APIKey      string              `bson:"api_key"`
	Model       string              `bson:"model"`
	BaseURL     string              `bson:"base_url"` // only meaningful for provider "openai"; empty = provider default
	IsActive    bool                `bson:"is_active"`
	Shared      bool                `bson:"shared"`
	OwnerUserID *primitive.ObjectID `bson:"owner_user_id,omitempty"`
	CreatedAt   time.Time           `bson:"created_at"`
}

// CreateAIConfig inserts a new saved provider config.
func (s *Store) CreateAIConfig(ctx context.Context, cfg AIConfig) (*AIConfig, error) {
	cfg.ID = primitive.NewObjectID()
	cfg.CreatedAt = time.Now()
	if _, err := s.aiConfigs.InsertOne(ctx, cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListAIConfigs returns every admin-owned (global) config, oldest first —
// used by the admin AI Providers tab. User-owned configs never appear here.
func (s *Store) ListAIConfigs(ctx context.Context) ([]AIConfig, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cursor, err := s.aiConfigs.Find(ctx, bson.M{"owner_user_id": bson.M{"$exists": false}}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	configs := []AIConfig{}
	if err := cursor.All(ctx, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// ListUserAIConfigs returns a user's own private configs, oldest first.
func (s *Store) ListUserAIConfigs(ctx context.Context, userID primitive.ObjectID) ([]AIConfig, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cursor, err := s.aiConfigs.Find(ctx, bson.M{"owner_user_id": userID}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	configs := []AIConfig{}
	if err := cursor.All(ctx, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// ListSharedAIConfigs returns every global config the admin has marked
// Shared — what an approved user can pick from.
func (s *Store) ListSharedAIConfigs(ctx context.Context) ([]AIConfig, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cursor, err := s.aiConfigs.Find(ctx, bson.M{"owner_user_id": bson.M{"$exists": false}, "shared": true}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	configs := []AIConfig{}
	if err := cursor.All(ctx, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// SetConfigShared toggles whether a global config is available to approved
// users. Meaningless on a user-owned config; callers only expose this for
// admin-owned ones.
func (s *Store) SetConfigShared(ctx context.Context, id primitive.ObjectID, shared bool) error {
	_, err := s.aiConfigs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"shared": shared}})
	return err
}

// GetAIConfig returns (nil, nil) — not an error — when there's no match.
func (s *Store) GetAIConfig(ctx context.Context, id primitive.ObjectID) (*AIConfig, error) {
	var cfg AIConfig
	err := s.aiConfigs.FindOne(ctx, bson.M{"_id": id}).Decode(&cfg)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// GetActiveAIConfig returns the currently-active config, or (nil, nil) if
// none is marked active yet (e.g. brand new install with nothing saved).
func (s *Store) GetActiveAIConfig(ctx context.Context) (*AIConfig, error) {
	var cfg AIConfig
	err := s.aiConfigs.FindOne(ctx, bson.M{"is_active": true}).Decode(&cfg)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// UpdateAIConfig edits a saved config. apiKey nil means "keep the existing
// key" — the same "blank means unchanged" convention used elsewhere, so the
// real key never has to round-trip back through an edit form.
func (s *Store) UpdateAIConfig(ctx context.Context, id primitive.ObjectID, name, model, baseURL string, apiKey *string) error {
	set := bson.M{"name": name, "model": model, "base_url": baseURL}
	if apiKey != nil {
		set["api_key"] = *apiKey
	}
	_, err := s.aiConfigs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	return err
}

// DeleteAIConfig removes a saved config. Callers are responsible for
// refusing to delete the active one (see AdminHandler.DeleteAIConfig).
func (s *Store) DeleteAIConfig(ctx context.Context, id primitive.ObjectID) error {
	_, err := s.aiConfigs.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// SetActiveAIConfig marks one global config active and every other global
// config inactive — scoped to owner_user_id absent so this never touches a
// user's private configs (which don't use IsActive at all).
func (s *Store) SetActiveAIConfig(ctx context.Context, id primitive.ObjectID) error {
	globalFilter := bson.M{"owner_user_id": bson.M{"$exists": false}}
	if _, err := s.aiConfigs.UpdateMany(ctx, globalFilter, bson.M{"$set": bson.M{"is_active": false}}); err != nil {
		return err
	}
	_, err := s.aiConfigs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"is_active": true}})
	return err
}

// RequestSharedAIAccess marks a user's request to use the shared/paid AI
// pool as pending — reviewed by an admin via SetSharedAIStatus.
func (s *Store) RequestSharedAIAccess(ctx context.Context, userID primitive.ObjectID) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": bson.M{"shared_ai_status": "pending"}})
	return err
}

// SetSharedAIStatus approves or rejects a user's shared-AI-pool request.
func (s *Store) SetSharedAIStatus(ctx context.Context, userID primitive.ObjectID, status string) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"_id": userID}, bson.M{"$set": bson.M{"shared_ai_status": status}})
	return err
}

// ListPendingAccessRequests returns users currently awaiting admin review —
// for the admin Users tab.
func (s *Store) ListPendingAccessRequests(ctx context.Context) ([]User, error) {
	cursor, err := s.users.Find(ctx, bson.M{"shared_ai_status": "pending"})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	users := []User{}
	if err := cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// SetSelectedAIConfig sets which config a user wants their chats to use.
// configID nil clears the selection, reverting to the app-wide default.
func (s *Store) SetSelectedAIConfig(ctx context.Context, userID primitive.ObjectID, configID *primitive.ObjectID) error {
	var update bson.M
	if configID == nil {
		update = bson.M{"$unset": bson.M{"selected_ai_config_id": ""}}
	} else {
		update = bson.M{"$set": bson.M{"selected_ai_config_id": *configID}}
	}
	_, err := s.users.UpdateOne(ctx, bson.M{"_id": userID}, update)
	return err
}
