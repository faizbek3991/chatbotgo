# Function-Calling Agent — Go + HTMX + MongoDB Atlas

A multi-provider AI chat app with function calling (tools), built with a
Go backend, an HTMX frontend (no build step, no JS framework), and
MongoDB Atlas for accounts, chat history, and an audit log.

Every user gets their own chat history, their own choice of AI provider
(their own API key, or an admin-approved shared one), and access to a set
of built-in tools the model can call automatically when it decides they're
useful — no need to ask for them by name.

## What it can do

- **Chat with history** — every conversation is saved per user, with
  auto-generated titles, and can be renamed or deleted from the sidebar.
- **Tool calling** — the model decides on its own when to call one of:
  - `get_weather` — current temperature & windspeed anywhere ([Open-Meteo](https://open-meteo.com), free)
  - `get_exchange_rate` — currency conversion ([frankfurter.app](https://frankfurter.app), free)
  - `eval_math` — arithmetic expressions like `(12 + 8) * 3 / 2`
  - `get_prayer_times` — Malaysian prayer times by state/district (JAKIM data)
  - `get_fuel_price` — this week's official Malaysian RON95/RON97/diesel prices
  - `get_public_holidays` — Malaysian public holidays by year/state
- **Bring your own AI** — every user can add their own provider config
  under **AI Settings** (`/settings`): Gemini, OpenAI (or any
  OpenAI-compatible endpoint — Groq, Together, SumoPod, local Ollama, etc.),
  or Anthropic, each with their own API key and model.
- **Shared AI pool** — instead of paying for their own key, a user can
  request access to the admin's shared provider(s). The admin approves or
  rejects the request manually (no payment processing built in — that
  happens outside the app); once approved, the user can pick any config
  the admin has marked "Shared".
- **Admin dashboard** (`/admin`, admin-only):
  - Promote/demote users between `user` and `admin`
  - Block/activate accounts (blocking takes effect immediately, even on
    an already-logged-in session)
  - Approve/reject shared-AI-pool access requests
  - Add, edit, test, and share/unshare AI provider configs
  - A live audit feed of every tool call any user has triggered

## Project layout

```
main.go                              entry point — config, Mongo, routes, middleware
internal/config/config.go            loads .env / environment variables
internal/db/mongo.go                 MongoDB Atlas: users, sessions, conversations, ai_configs, tool_calls
internal/auth/                       bcrypt passwords, session tokens, RequireAuth/RequireAdmin middleware
internal/agent/agent.go              the agentic tool-calling loop
internal/agent/gemini.go             Gemini client
internal/agent/openai.go             OpenAI / OpenAI-compatible client
internal/agent/anthropic.go          Anthropic (Claude) client
internal/agent/provider.go           picks the right client for a config; connection test helper
internal/agent/tools.go              tool declarations + implementations
internal/agent/mathexpr.go           dependency-free arithmetic expression parser
internal/handlers/auth.go            signup / login / logout
internal/handlers/chat.go            chat + conversation CRUD, resolves which AI client to use per user
internal/handlers/settings.go        per-user AI provider settings (/settings)
internal/handlers/admin.go           admin dashboard (/admin)
templates/                           HTML templates (HTMX fragments, no JS framework)
static/style.css                     navy/teal theme
```

## Setup

**1. Get a free Gemini API key**
[Google AI Studio](https://aistudio.google.com/apikey) → Create API Key.
Free tier: 1,500 requests/day on `gemini-2.5-flash`. (You can add other
providers later from inside the app — this is just the app-wide default.)

**2. Create a free MongoDB Atlas cluster**
[mongodb.com/atlas](https://www.mongodb.com/atlas) → free M0 cluster →
Database Access (create a user) → Network Access (allow your IP, or
`0.0.0.0/0` for local dev — see the security note below) → Connect →
Drivers → copy the connection string.

**3. Configure environment**
```bash
cp .env.example .env
# edit .env: GEMINI_API_KEY, MONGODB_URI, and ADMIN_EMAIL
```
`ADMIN_EMAIL` is whichever email should get the admin role automatically
when it signs up. Set it to your own email before the first run — there's
no other way to become the first admin.

**4. Install dependencies and run**
```bash
go mod tidy
go run main.go
```
Open http://localhost:8080 → you'll land on `/login` → click "Sign up" and
register with the email you put in `ADMIN_EMAIL` to get admin access.
Everyone else who signs up gets the `user` role by default.

## Using it day to day

- **Chat**: type a message — ask about weather, an exchange rate, a math
  expression, prayer times, fuel prices, or Malaysian holidays, and the
  model will call the relevant tool on its own when it's useful.
- **Switch AI provider**: go to **AI Settings** (top nav) → add your own
  provider + API key, or request access to the shared pool → pick "Use
  this" on whichever config you want your chats to use. Selecting nothing
  just uses the app-wide default.
- **Admin tasks**: go to `/admin` → Users tab to manage roles/blocking and
  approve shared-AI requests, AI Providers tab to add/share provider
  configs, Recent tool calls tab to see what's being used across the app.

## Security notes

- `MONGODB_URI` and every provider API key in `.env` (or entered under
  Settings/Admin) are real credentials, not scoped tokens — anyone who
  gets your Mongo connection string can connect to the database directly
  with any MongoDB client, bypassing the app entirely. Keep Atlas's
  Network Access list as tight as you can, and never commit `.env`
  (already covered by `.gitignore`).
- If you ever paste a real secret into a chat, log, or debugging tool
  (including an AI assistant), treat it as exposed and rotate it —
  regenerate the key/password rather than assume it's fine because it
  wasn't committed anywhere.
- Sessions are opaque random tokens stored in a MongoDB `sessions`
  collection (TTL-indexed, so Mongo expires them on its own), not JWTs —
  `/logout` deletes the session document, which revokes it instantly.
  Blocking a user is similarly enforced on every request, not just login.

## Known limits (by design)

- Tool-call badges from history aren't re-rendered on page reload — only
  live turns show them. The raw log is still queryable via `tool_calls`.
- `maxSteps = 5` caps a single turn's tool-call chain; exceeding it is a
  hard error.
- No streaming — each turn waits for the model's full response.
- Shared-AI access approval is a manual admin flag, not a real payment
  gateway — how/whether payment happens outside the app is up to you.
