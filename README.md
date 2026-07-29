# Function-Calling Agent — Go + HTMX + MongoDB Atlas

A Gemini agent with function calling, ported from the Node.js/Gemini
pattern to a Go backend, an HTMX frontend (no build step, no JS framework),
and MongoDB Atlas for conversation history + a tool-call audit log.

Same agentic loop as the original: send the conversation plus a list of
tool schemas to Gemini → if it responds with a `functionCall` instead of
text, run the matching Go function → send the result back → repeat until
Gemini answers in plain text.

Tools included (all free, no API key required):
- `get_weather` — [Open-Meteo](https://open-meteo.com)
- `get_exchange_rate` — [frankfurter.app](https://frankfurter.app)
- `eval_math` — a small hand-rolled arithmetic parser (`internal/agent/mathexpr.go`)

**Now includes accounts and roles:**
- Sign up / log in / log out, with bcrypt-hashed passwords
- Server-side sessions in MongoDB Atlas (a `sessions` collection with a TTL
  index — Mongo deletes expired sessions on its own, no cron job needed)
- Two roles: `user` (default) and `admin`, gated with route middleware
- `/admin` — user list + a live audit feed of every tool call across every
  user, admin-only

## Project layout

```
main.go                       entry point — wires config, Mongo, agent, routes, middleware
internal/config/config.go     loads .env / environment variables
internal/db/mongo.go          MongoDB Atlas: users, sessions, conversations, tool call log
internal/auth/password.go     bcrypt password hashing
internal/auth/token.go        random session token generator
internal/auth/context.go      RequireAuth / RequireAdmin middleware + context helpers
internal/agent/gemini.go      Gemini REST client (generateContent + tools)
internal/agent/tools.go       tool declarations + implementations
internal/agent/mathexpr.go    dependency-free arithmetic expression parser
internal/agent/agent.go       the agentic loop (function-call negotiation)
internal/handlers/auth.go     signup / login / logout handlers
internal/handlers/admin.go    admin dashboard handler
internal/handlers/chat.go     chat handlers, now keyed off the logged-in user
templates/index.html          chat page shell (with user header)
templates/turn.html           fragment appended by HTMX after each message
templates/login.html          login page
templates/signup.html         signup page
templates/admin.html          admin dashboard: users + tool call audit trail
static/style.css              navy/teal theme
```

## Setup

**1. Get a free Gemini API key**
[Google AI Studio](https://aistudio.google.com/apikey) → Create API Key.
Free tier: 1,500 requests/day on `gemini-2.0-flash`.

**2. Create a free MongoDB Atlas cluster**
[mongodb.com/atlas](https://www.mongodb.com/atlas) → free M0 cluster →
Database Access (create a user) → Network Access (allow your IP, or
`0.0.0.0/0` for local dev) → Connect → Drivers → copy the connection string.

**3. Configure environment**
```bash
cp .env.example .env
# edit .env: GEMINI_API_KEY, MONGODB_URI, and ADMIN_EMAIL
```
`ADMIN_EMAIL` is whichever email should get the admin role automatically
when it signs up. Set it to your own email before the first run.

**4. Install the external dependencies**
```bash
go get go.mongodb.org/mongo-driver/mongo
go get golang.org/x/crypto/bcrypt
go mod tidy
```
(This was scaffolded in a sandbox without internet access to the Go module
proxy, so go.mod intentionally has no pinned versions yet — `go get` will
fetch the current ones.)

**5. Run**
```bash
go run main.go
```
Open http://localhost:8080 → you'll land on `/login` → click "Sign up" and
register with the email you put in `ADMIN_EMAIL` to get admin access, then
visit `/admin`. Everyone else who signs up gets the `user` role.

## Teaching notes

- **The negotiation, made visible**: `internal/agent/agent.go` is short
  enough to read end-to-end in class. Point out that `call == nil` is the
  only exit condition — everything else is "the model asked for a tool,
  so we ran it and told it what happened."
- **Wire format over SDK**: `gemini.go` talks to the raw
  `v1beta/models/{model}:generateContent` REST endpoint instead of a
  Google SDK. Good for showing students exactly what JSON crosses the
  wire (`functionCall` / `functionResponse` parts) — the same shape
  they'd see in any language.
- **Why MongoDB Atlas here**: two collections — `conversations` (so a
  page refresh doesn't lose the chat) and `tool_calls` (an audit trail:
  which tool, what args, what came back, when). Good jumping-off point
  for a "show your work" panel exercise.
- **Session model**: a random cookie, no login — swap in real auth for a
  multi-user version.
- **Extending it**: adding a fourth tool means (1) a `FunctionDeclaration`
  in `tools.go`, (2) a Go function matching `ToolFunc`, (3) one line in
  `Registry()`. Nothing else in the loop changes — a good live-coding
  exercise.
- **Sessions vs JWT**: this uses opaque tokens looked up against a
  `sessions` collection, not JWTs. Worth contrasting in class — a stolen
  JWT is valid until it expires no matter what you do; a stolen session
  token here can be killed instantly by deleting its document (which is
  exactly what `/logout` does).
- **Changing someone's role after launch**: there's no admin UI for this
  yet — update it directly in Atlas (`users` collection → edit the `role`
  field on a document → `"admin"` or `"user"`). A good "add a feature"
  exercise: build a button in `/admin` that does this via a form post.
- **Route protection pattern**: `auth.RequireAuth` and `auth.RequireAdmin`
  in `internal/auth/context.go` are plain function-wrapping middleware —
  no framework needed. `RequireAdmin` literally wraps `RequireAuth`, which
  is a clean way to show middleware composition.

## Known limits (by design, for clarity)

- Tool-call badges from history aren't re-rendered on page reload (only
  live turns show them) — the raw log is still in `tool_calls` if you want
  to extend `Index` to include them.
- `maxSteps = 5` in `agent.go` caps a single turn's tool-call chain.
- No streaming — each turn waits for Gemini's full response.
