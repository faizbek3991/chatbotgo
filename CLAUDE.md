# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Gemini function-calling agent, ported from the Node.js/Gemini tutorial pattern to a Go backend with an HTMX frontend (no build step, no JS framework) and MongoDB Atlas for conversation history + a tool-call audit log. It also layers on accounts/roles (signup/login, bcrypt passwords, server-side sessions, `user`/`admin` roles).

## Setup and running

```bash
cp .env.example .env   # fill in GEMINI_API_KEY, MONGODB_URI, ADMIN_EMAIL
go get go.mongodb.org/mongo-driver/mongo
go get golang.org/x/crypto/bcrypt
go mod tidy
go run main.go
```

- `go.mod` intentionally has no pinned dependency versions — this was scaffolded without access to `proxy.golang.org`. Run `go get`/`go mod tidy` after cloning, or after adding any new dependency.
- No test suite exists yet.
- `ADMIN_EMAIL` in `.env` controls which signup email gets the `admin` role automatically. There is no admin UI to change roles later — edit the `role` field on a document in Atlas's `users` collection directly.
- Server listens on `PORT` (default 8080). Visit `/login` → sign up → `/` for chat, `/admin` for the admin dashboard (admin-only).

## Architecture

The core interaction is the **agentic loop** in [internal/agent/agent.go](internal/agent/agent.go): send the conversation + tool schemas to Gemini → if it responds with a `functionCall` part instead of text, run the matching Go function, append the result as a `function`-role turn, and repeat → until Gemini answers in plain text (`call == nil`). Capped at `maxSteps = 5` per turn. `gemini.go` talks directly to the raw `v1beta/models/{model}:generateContent` REST endpoint (no Google SDK), so `functionCall`/`functionResponse` parts are the literal wire format.

Adding a new tool requires exactly three edits, nothing else in the loop changes:
1. A `FunctionDeclaration` in [internal/agent/tools.go](internal/agent/tools.go)'s `Declarations()`.
2. A Go function matching the `ToolFunc` signature (`func(ctx, args map[string]interface{}) (map[string]interface{}, error)`).
3. One line wiring the name to the function in `Registry()`.

Existing tools are all free/keyless: `get_weather` (Open-Meteo), `get_exchange_rate` (frankfurter.app), `eval_math` (hand-rolled recursive-descent parser in [internal/agent/mathexpr.go](internal/agent/mathexpr.go), no external dependency).

**Auth/session model**: opaque random session tokens looked up against a MongoDB `sessions` collection (TTL-indexed, so Mongo expires them itself) — not JWTs. `/logout` deletes the session document, which invalidates it instantly (a stolen JWT can't be revoked this way). `internal/auth/context.go` has `RequireAuth`/`RequireAdmin` as plain function-wrapping middleware (no framework); `RequireAdmin` wraps `RequireAuth`.

**MongoDB collections** (see [internal/db/mongo.go](internal/db/mongo.go)): `users`, `sessions` (TTL-indexed), `conversations` (per-user chat history, survives page refresh), `tool_calls` (audit log of every tool invocation across all users — which tool, args, result, timestamp; surfaced live in `/admin`).

**Request flow**: `main.go` wires config → Mongo connection → `agent.NewAgent(geminiClient)` → handlers → `net/http` `ServeMux` with Go 1.22 method+pattern routing (`GET /{$}`, `POST /chat`, etc.) → HTMX fragments (`templates/turn.html`) appended to the chat page after each turn.

## Known limits (by design)

- Tool-call badges from history aren't re-rendered on page reload — only live turns show them. The raw log is still queryable in `tool_calls` if extending `Index` to include them.
- `maxSteps = 5` caps a single turn's tool-call chain; exceeding it is a hard error.
- No streaming — each turn waits for Gemini's full response.
