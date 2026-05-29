# Passkey Authentication — Implementation Plan

**Status:** Proposed (awaiting review). No code written yet.
**Author:** design session 2026-05-29.
**Reference:** modelled on `../terminal-hub`'s M3 auth design
(`docs/superpowers/plans/2026-05-21-m3-auth-single-user.md`), adapted from Rust/
`webauthn-rs` to Go/`go-webauthn`.

---

## 1. Goal

Add **WebAuthn passkey login** to ai-dev-conductor as the recurring credential,
with the existing shared password demoted to an **admin / break-glass bootstrap
secret**. Introduce a real **multi-user** model (accounts + per-user passkeys)
backed by persistent storage, without breaking the current single-binary release
or the multi-server frontend.

### Decisions locked in this session

| Decision | Choice | Consequence |
|----------|--------|-------------|
| Bootstrap factor | **Existing password** | `AI_CONDUCTOR_PASSWORD` becomes the admin/break-glass secret. No SSH-key/ssh-agent machinery. |
| User scope | **Multi-user with accounts** | New `users` + `credentials` tables; an admin creates accounts and mints enroll links. |
| Deliverable now | **This plan** | Implementation follows after review. |

### Non-goals (this round)

- Cross-server passkeys (see §9 — passkeys bind to one origin; remote servers keep token auth).
- Account self-signup without an admin (admin mints enroll links instead).
- Hardware-attestation / FIDO MDS policy enforcement (accept any platform/roaming authenticator).

---

## 2. The enabling fact and the hard constraint

**Enabler.** WebAuthn needs a secure context and a real RP ID. As of 2026-05-29
the app is reachable at **`https://home.cloudlabs.live`** (nginx-terminated TLS,
forward set up the same day). So:

- `RPID = "home.cloudlabs.live"`
- `RPOrigins = ["https://home.cloudlabs.live"]`

Browsers also exempt `http://localhost`, so local dev keeps working with
`RPID = "localhost"` selected by config.

**Constraint — passkeys vs. the multi-server model.** A passkey is bound to **one**
RP ID. The frontend's `fetchFromServer(server, …)` bearer-token model logs into N
backends; a passkey minted on `home.cloudlabs.live` authenticates **only** to that
origin. Therefore:

- Passkeys replace the **local/primary** server's password login.
- **Remote** servers in the sidebar keep the existing token-paste auth, OR each is
  fronted by its own HTTPS domain and enrolls its own passkeys independently.

This is called out in the UI plan (§8) so users aren't surprised.

---

## 3. Architecture decisions

### 3.1 Library — `github.com/go-webauthn/webauthn`

The de-facto Go WebAuthn library; direct analogue of `webauthn-rs`. We implement
the `webauthn.User` interface and use the four ceremony calls:

```
w, _ := webauthn.New(&webauthn.Config{
    RPID:          cfg.RPID,                 // "home.cloudlabs.live" | "localhost"
    RPDisplayName: "AI Dev Conductor",
    RPOrigins:     cfg.RPOrigins,            // ["https://home.cloudlabs.live"]
})
options, sessionData, _ := w.BeginRegistration(user, ...)   // -> JSON to browser + state to stash
cred, _              := w.FinishRegistration(user, sessionData, httpReq)
options, sessionData, _ := w.BeginLogin(user)
cred, _              := w.FinishLogin(user, sessionData, httpReq)
```

`BeginRegistration`/`BeginLogin` return `*protocol.CredentialCreation` /
`*protocol.CredentialAssertion`, which serialise to exactly the JSON
`navigator.credentials.create()/get()` expect (base64url already handled).
`Finish*` parse the browser response straight from `*http.Request`.

### 3.2 Persistence — `modernc.org/sqlite` (pure Go, no cgo)

The project has **no DB today** and ships static binaries. `modernc.org/sqlite`
keeps `CGO_ENABLED=0` working, so the release story is unchanged. (Reject `mattn/
go-sqlite3` — it needs cgo. `bbolt` is a fallback if we want zero SQL, but SQLite
ages better and mirrors terminal-hub.)

DB path: `<DataDir>/state.db` (DataDir already exists in config, default
`./data/sessions`). WAL mode, `foreign_keys=ON`.

### 3.3 Coexistence with the password

`AuthService` (bcrypt) stays. The shared password becomes the **admin secret**:

- First run / no users: logging in with the password yields an **admin session**.
- Admin can create user accounts and mint one-time enroll links.
- Admin can also self-enroll a passkey bound to an `admin` account.
- The password remains a **break-glass** login (config flag
  `AI_CONDUCTOR_PASSWORD_LOGIN=on|admin-only|off`, default `on`).

### 3.4 Reverse proxy

nginx terminates TLS and proxies the host root → `:5050`, so the app sees
`X-Forwarded-Proto: https`. We trust it (behind our own nginx) to set the session
cookie `Secure`. RP config is static, so no host-header trust needed for WebAuthn
itself. Cookie stays `HttpOnly; SameSite=Strict; Path=/`.

---

## 4. Data model

`internal/store/migrations/0001_init.sql`:

```sql
CREATE TABLE IF NOT EXISTS users (
  id           TEXT PRIMARY KEY,          -- random 16-byte, hex; this is the WebAuthn user handle
  username     TEXT NOT NULL UNIQUE,      -- login identifier (email or handle)
  display_name TEXT NOT NULL,
  role         TEXT NOT NULL CHECK(role IN ('admin','user')),
  created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS credentials (
  id            BLOB PRIMARY KEY,         -- raw credential ID
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  public_key    BLOB NOT NULL,
  attestation   TEXT,                     -- raw type / AAGUID for audit
  sign_count    INTEGER NOT NULL DEFAULT 0,
  transports    TEXT,                     -- JSON array
  cred_blob     BLOB NOT NULL,            -- full JSON-marshalled webauthn.Credential (source of truth)
  nickname      TEXT,                     -- "MacBook Touch ID"
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_credentials_user ON credentials(user_id);

CREATE TABLE IF NOT EXISTS enroll_tokens (   -- admin-minted one-time enroll links
  token_hash  BLOB PRIMARY KEY,             -- argon2id hash of the raw token
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issued_at   INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  consumed_at INTEGER
);

CREATE TABLE IF NOT EXISTS sessions (        -- replaces in-memory SessionStore (survives restart)
  cookie_hash  BLOB PRIMARY KEY,             -- sha256 of cookie value
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issued_at    INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS audit_log (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,
  user_id    TEXT,
  action     TEXT NOT NULL,                  -- login, passkey-register, enroll-mint, account-create, ...
  details    TEXT
);
```

In-flight WebAuthn ceremony state (`webauthn.SessionData`) is **in-memory**, keyed
by a random UUID held in a short-lived cookie (`th_ceremony`), 5-min TTL — never
persisted (it's a one-shot nonce). Mirrors terminal-hub's `reg_state`/`auth_state`.

---

## 5. Module layout (Go)

```
internal/store/          NEW — SQLite store + migrations (Users, Credentials, EnrollTokens, Sessions, Audit)
  store.go
  migrations/0001_init.sql
internal/auth/
  auth.go                KEEP — bcrypt admin password
  middleware.go          MODIFY — session lookup hits store; RequireAuth + RequireAdmin
  webauthn.go            NEW — wraps go-webauthn; implements webauthn.User over a store user
  ceremony.go            NEW — in-memory SessionData cache (UUID -> {userID, data, expiry})
api/
  handlers.go            MODIFY — password login issues store-backed session
  passkey.go             NEW — register/begin|finish, login/begin|finish, logout
  admin.go               NEW — create account, mint enroll link, list/revoke credentials
config/config.go         MODIFY — add RPID, RPOrigins, PasswordLoginMode
web/templates/
  login.html             MODIFY — passkey login + password fallback
  enroll.html            NEW — passkey registration page (opened via enroll link or by logged-in user)
web/static/js/
  webauthn.js            NEW — base64url helpers + navigator.credentials.* (ported from terminal-hub login.js/enroll.js)
```

---

## 6. Endpoints

Public (no session required):
```
POST /api/login                      password -> admin/user session (existing, modified)
POST /auth/passkey/login/begin       {username}                -> CredentialAssertion JSON + ceremony cookie
POST /auth/passkey/login/finish      navigator.credentials.get -> sets session cookie
GET  /enroll                         ?t=<token>  enrollment page (token redeemed at register/begin)
```
Authenticated (valid session) or token-gated:
```
POST /auth/passkey/register/begin    (session OR ?t=token)     -> CredentialCreation JSON + ceremony cookie
POST /auth/passkey/register/finish   navigator.credentials.create -> stores credential
POST /auth/logout                    clears + deletes session
```
Admin only:
```
POST   /api/admin/users              {username, display_name, role} -> creates account
POST   /api/admin/users/{id}/enroll  -> mints one-time enroll link (5-min, argon2-hashed)
GET    /api/admin/users              list users + credential counts
DELETE /api/admin/credentials/{id}   revoke a passkey
```

All `/auth/*` and `/api/login` and `/enroll` are exempt from `RequireAuth`;
everything else (including `/ws/{id}`) stays gated. `/api/admin/*` adds `RequireAdmin`.

---

## 7. Task-by-task

> Each task ends green (`go build ./... && go vet ./... && go test ./...`) and is a
> separate commit. Work on a branch `feat/passkey-auth`.

### Task 1 — SQLite store + migrations
- Add `modernc.org/sqlite`, `golang.org/x/crypto/argon2` (or `argon2id` helper).
- `internal/store/store.go`: `Open(path)`, embedded migration runner (`_migrations`
  table, idempotent), and typed methods: `CreateUser`, `GetUserByUsername`,
  `GetUserByID`, `AddCredential`, `CredentialsForUser`, `UpdateCredential`,
  `DeleteCredential`, `MintEnrollToken`, `RedeemEnrollToken`, `InsertSession`,
  `LookupSession`, `DeleteSession`, `Audit`.
- **Tests:** migrations idempotent; user round-trip; enroll-token consume-once;
  expired session not found. (Use `:memory:` or a tempfile.)

### Task 2 — config additions
- `config.go`: `RPID`, `RPOrigins []string`, `PasswordLoginMode`, derived from env:
  - `AI_CONDUCTOR_RP_ID` (default `localhost`)
  - `AI_CONDUCTOR_PUBLIC_URL` (e.g. `https://home.cloudlabs.live`) → derives RPID + origin if RP_ID unset
  - `AI_CONDUCTOR_PASSWORD_LOGIN` (`on` default).
- `Validate()`: if RPID != localhost, origin must be https.
- **Tests:** public-url → rpid/origin derivation; localhost dev default.

### Task 3 — store-backed sessions + middleware
- `auth/middleware.go`: `SessionStore` gains a store-backed impl (sha256(cookie) →
  `sessions` row). Keep the same `Validate`/`Add`/`Remove` surface so callers don't
  churn. `RequireAuth` unchanged in shape; add `RequireAdmin` (looks up role).
- Migrate `/api/login` to issue a store session bound to a user (admin user row
  auto-created on first password login if none exists: username `admin`, role `admin`).
- **Tests:** session survives a store reopen; admin-only route 403s for `user` role.

### Task 4 — go-webauthn wrapper + ceremony cache
- `auth/webauthn.go`: `type WebUser struct{ ... }` implementing `webauthn.User`
  (`WebAuthnID`=user.id bytes, `WebAuthnName`=username, `WebAuthnDisplayName`,
  `WebAuthnCredentials`=decoded `cred_blob`s, `WebAuthnIcon`=""). `New(cfg, store)`.
- `auth/ceremony.go`: in-memory `map[uuid]{userID, *webauthn.SessionData, expiry}`,
  5-min TTL, GC on access; `th_ceremony` cookie carries the UUID.
- **Tests:** ceremony put/get-once/expiry. (Full WebAuthn needs a browser → §7 Task 8.)

### Task 5 — passkey HTTP handlers
- `api/passkey.go`: the four ceremony endpoints + logout, wired to store + wrapper.
  - register/begin: resolve user from session **or** redeemed enroll token; exclude
    existing creds; stash SessionData; return options.
  - register/finish: `FinishRegistration`; persist credential; audit `passkey-register`.
  - login/begin: `BeginLogin(user)`; stash; return options.
  - login/finish: `FinishLogin`; on success update sign_count, issue session cookie
    (`Secure` from X-Forwarded-Proto), audit `passkey-login`.
- **Tests:** begin returns well-formed options; login/begin for unknown user is a
  generic 401 (no user enumeration).

### Task 6 — admin handlers
- `api/admin.go`: create account, mint enroll link (returns `…/enroll?t=<raw>` once),
  list users, revoke credential. All behind `RequireAdmin`. Audit each.
- **Tests:** non-admin 403; enroll token single-use; revoke removes credential.

### Task 7 — frontend
- `web/static/js/webauthn.js`: base64url↔ArrayBuffer + `prepCreate`/`prepRequest`
  (ported from terminal-hub `login.js`/`enroll.js`), and `registerPasskey()` /
  `loginWithPasskey(username)`.
- `login.html`: username field + **"Sign in with passkey"** (primary) and a
  collapsible **password** fallback (admin/break-glass). On password success,
  offer "Add a passkey to this account".
- `enroll.html`: reads `?t=`, calls register/begin→`create()`→register/finish, then
  redirects to `/terminal`.
- **Manual smoke** on `https://home.cloudlabs.live` (real authenticator).

### Task 8 — end-to-end test (virtual authenticator)
- Playwright (the repo already uses it for paste e2e) with
  `CDP Authenticator` (`WebAuthn.addVirtualAuthenticator`) to drive
  register→login→reach `/terminal`. Covers what unit tests can't.
- Also a Go integration test: password login → mint enroll token → register/begin
  accepts token once, rejects twice (mirrors terminal-hub `tests/auth.rs`).

### Task 9 — docs + ops
- README: passkey section, RP_ID / PUBLIC_URL env vars, "must be reached via the
  HTTPS domain, not the LAN IP" note, break-glass password behaviour.
- Note the `state.db` is now a stateful file to back up (it holds users + creds).

---

## 8. Security notes / threat model

- **RP ID pinning** to `home.cloudlabs.live` means a phisher on another domain can't
  use the passkey — the core WebAuthn guarantee. Verify origin strictly (no
  wildcard origins).
- **No user enumeration:** login/begin and enroll endpoints return generic errors
  and constant-ish timing.
- **Enroll tokens:** argon2id-hashed at rest, single-use, 5-min TTL — same shape as
  terminal-hub bootstrap tokens.
- **Sign-count regression** → flag/refuse (cloned-authenticator signal).
- **Break-glass password:** keep it strong; consider `PASSWORD_LOGIN=admin-only`
  once every human has a passkey, and document rotating `AI_CONDUCTOR_PASSWORD`.
- **Cookie `Secure`** depends on trusting `X-Forwarded-Proto` from *our* nginx only —
  fine here; document that the app must sit behind the TLS proxy in prod.

---

## 9. Open questions for review

1. **Account creation UX:** admin-mints-links only, or also allow a logged-in user
   to add more passkeys to their *own* account (recommended yes — multi-device)?
2. **Remote servers in the sidebar:** confirm they stay token-auth (passkeys are
   per-origin) — or do you want each remote behind its own domain + passkeys later?
3. **Username = email?** Affects display and whether we validate format.
4. **Keep password login on indefinitely**, or auto-flip to `admin-only` once a
   user has ≥1 passkey?
```
