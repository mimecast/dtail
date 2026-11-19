# Auth-Key Fast-Reconnect for DTail

## Problem

When using a YubiKey for SSH authentication, each DTail connection requires a
physical touch of the YubiKey during the SSH handshake. This is slow and becomes
painful when connecting to many servers concurrently — the YubiKey serialises
all signing requests, turning parallel connections into sequential ones.

## Solution

Allow the DTail client to register a local SSH public key with the DTail server
over an already-authenticated SSH session. The server caches this key
**in-memory only** (never written to disk). On subsequent connections the client
offers that local key first — a pure in-memory RSA verify with no YubiKey
interaction — and falls back to the original auth method if the server does not
recognise the key.

## Design Principles

1. **Transparent fallback** — Go's `golang.org/x/crypto/ssh` tries each
   `AuthMethod` in order; if the fast key is rejected the client silently falls
   back to the SSH agent / YubiKey. No user interaction required.
2. **Server keys are ephemeral** — the in-memory store is lost on server
   restart. No file I/O, no persistence.
3. **Trust chain preserved** — an auth-key can only be registered over a session
   that was already authenticated via the normal (YubiKey) path.
4. **Minimal protocol addition** — a single `AUTHKEY <base64-pubkey>` command
   sent over the existing SSH session text protocol.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│ DTail Client                                            │
│                                                         │
│  Auth methods (tried in order):                         │
│    1. Local private key (~/.ssh/id_rsa)  ← FAST         │
│    2. SSH Agent / YubiKey               ← SLOW fallback │
│                                                         │
│  After slow-path auth:                                  │
│    → sends AUTHKEY <~/.ssh/id_rsa.pub> to server        │
└────────────────────────┬────────────────────────────────┘
                         │ SSH
┌────────────────────────▼────────────────────────────────┐
│ DTail Server (dserver)                                  │
│                                                         │
│  PublicKeyCallback:                                     │
│    1. Check in-memory authkeystore  ← FAST              │
│    2. Check authorized_keys file    ← existing path     │
│                                                         │
│  AUTHKEY command handler:                               │
│    → authkeystore.Add(user, pubkey)                     │
│    → responds AUTHKEY OK / AUTHKEY ERR                  │
│                                                         │
│  authkeystore (in-memory only):                         │
│    map[username] → []PublicKey (with TTL, max per user) │
└─────────────────────────────────────────────────────────┘
```

## Sequence of Events

### First Connection (slow path — YubiKey)

1. Client checks for local private key at `~/.ssh/id_rsa` (or `--auth-key-path`).
2. Client builds auth methods list: `[localKey, sshAgent]`.
3. SSH handshake begins; server's `PublicKeyCallback` is called with local key.
4. Server checks in-memory authkeystore → not found.
5. Server checks `authorized_keys` file → not found (this key isn't in there).
6. Server rejects the key.
7. Go SSH client automatically tries next auth method: SSH agent (YubiKey).
8. YubiKey signs the challenge; server finds the YubiKey pubkey in
   `authorized_keys` → auth succeeds.
9. Session is established; client sends DTail commands as usual.
10. Client reads `~/.ssh/id_rsa.pub` and sends `AUTHKEY <base64-pubkey>`.
11. Server's handler parses the command, calls `authkeystore.Add(user, pubkey)`.
12. Server responds `AUTHKEY OK`.

### Subsequent Connections (fast path — no YubiKey)

1. Client builds auth methods list: `[localKey, sshAgent]`.
2. SSH handshake begins; server's `PublicKeyCallback` is called with local key.
3. Server checks in-memory authkeystore → **found** → auth succeeds immediately.
4. No YubiKey touch needed. Session is established instantly.

### Fallback (server restarted, key expired)

1. Client offers local key → server's authkeystore is empty → rejected.
2. Client falls back to SSH agent → YubiKey auth succeeds.
3. Client re-registers local pubkey via `AUTHKEY` command.

## Components

### 1. Server: In-Memory Auth-Key Store

**New file:** `internal/ssh/server/authkeystore.go`

- Thread-safe store using `sync.RWMutex`.
- Data structure: `map[string][]authKeyEntry` where key is username.
- Each `authKeyEntry` holds `gossh.PublicKey` + `time.Time` (registered at).
- Methods: `Add(user, pubkey)`, `Has(user, pubkey) bool`, `Remove(user, pubkey)`.
- Per-user max key limit (default 5, configurable via `AuthKeyMaxPerUser`).
- TTL-based expiry (default 24h, configurable via `AuthKeyTTLSeconds`).
- Lazy expiry: check TTL on `Has()` calls; optionally a background reaper.
- Package-level singleton or passed via dependency injection.

### 2. Server: Extend PublicKeyCallback

**Modified file:** `internal/ssh/server/publickeycallback.go`

- Before the existing `authorizedKeysFile` lookup, check `authkeystore.Has(user, offeredPubKey)`.
- If found → return success immediately (fast path).
- If not found → fall through to existing file-based logic (no behaviour change).

### 3. Server: AUTHKEY Command Handler

**Modified file:** `internal/server/handlers/serverhandler.go` (or relevant handler)

- Parse incoming line for `AUTHKEY <base64-pubkey>` prefix.
- Decode the base64 public key using `gossh.ParsePublicKey()`.
- Call `authkeystore.Add(user, pubkey)`.
- Write `AUTHKEY OK\n` or `AUTHKEY ERR <reason>\n` back to the client.
- Guard: only accept if `AuthKeyEnabled` is true in server config.

### 4. Client: Auth Method Ordering (Multi-Method Support)

**Modified file:** `internal/ssh/client/authmethods.go`

- Change `initKnownHostsAuthMethods` to **collect multiple auth methods**
  instead of returning after the first successful one.
- Order: local private key first (from `--auth-key-path`, default `~/.ssh/id_rsa`),
  then SSH agent, then other default keys.
- This ensures Go's SSH client tries the fast key before the YubiKey.

### 5. Client: Auth-Key Registration After Slow-Path Connection

**Modified file:** `internal/clients/connectors/serverconnection.go` (or handler layer)

- After session is established and DTail commands are sent, determine whether
  the connection used the fast path or slow path.
- If slow path (YubiKey was used): read the public key file
  (`--auth-key-path` + `.pub`), send `AUTHKEY <base64-pubkey>` command.
- Parse `AUTHKEY OK` / `AUTHKEY ERR` response.
- A simple heuristic: if the auth-key-path private key exists and we have a
  corresponding `.pub` file, always send the registration — sending it again is
  idempotent and cheap.

### 6. Configuration

**Modified files:** `internal/config/server.go`, `internal/config/client.go`, `internal/config/args.go`

Server config (`dtail.json`):
- `AuthKeyEnabled` (bool, default `true`)
- `AuthKeyTTLSeconds` (int, default `86400` = 24h)
- `AuthKeyMaxPerUser` (int, default `5`)

Client config / CLI flags:
- `--auth-key-path` (string, default `~/.ssh/id_rsa`) — path to the local
  private key to try first and whose `.pub` counterpart is registered
- `--no-auth-key` (bool, default `false`) — disable auth-key feature entirely

### 7. Integration Tests

**Modified/new files in:** `integrationtests/`

- Test that auth-key registration works end-to-end.
- Test that fast-path auth succeeds after registration.
- Test fallback when server has no cached key (simulating restart).
- Test TTL expiry and max-keys-per-user limits.
- Test `--no-auth-key` disables the feature.

### 8. Documentation

- Update `README.md` with auth-key feature description.
- Update `AGENTS.md` / `CLAUDE.md` with new config options and architecture notes.

## Security Considerations

- **No server-side disk persistence** — keys exist only in memory, lost on restart.
- **Trust chain** — auth-keys can only be registered over an already-authenticated
  session. An attacker cannot register a key without first proving identity.
- **TTL expiry** — keys auto-expire (default 24h), limiting exposure window.
- **Per-user limits** — max 5 keys per user prevents memory exhaustion.
- **Same security model as `~/.ssh/id_rsa`** — the local key is protected by
  filesystem permissions (0600). If an attacker has access to `~/.ssh/id_rsa`,
  they already have SSH access anyway.
- **No new attack surface** — the `AUTHKEY` command is only processed inside an
  authenticated session. The `PublicKeyCallback` fast-path is equivalent to
  having the key in `authorized_keys`.

## Implementation Order

1. Auth-key store (server, standalone, unit-testable)
2. Extend `PublicKeyCallback` (server, minimal change)
3. `AUTHKEY` command handler (server handler)
4. Client auth method ordering (multi-method collection)
5. Client auth-key registration (send pubkey after slow-path)
6. Configuration and CLI flags
7. Integration tests
8. Documentation
