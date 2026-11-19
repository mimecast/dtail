# Auth-Key Fast-Reconnect for DTail

## Problem

When using a YubiKey for SSH authentication, each DTail connection requires a
physical touch of the YubiKey during the SSH handshake. This is slow and becomes
painful when connecting to many servers concurrently — the YubiKey serialises
all signing requests, turning parallel connections into sequential ones.

## Solution

The DTail client registers a local SSH public key with the DTail server over an
already-authenticated SSH session. The server caches this key **in-memory only**
(never written to disk). On subsequent connections the client offers that local
key first — a pure in-memory RSA verify with no YubiKey interaction — and falls
back to the original auth method if the server does not recognise the key.

The feature is enabled by default. It can be turned off on the client with
`--no-auth-key` (`Client.AuthKeyDisable` in the client configuration) and on the
server with `Server.AuthKeyEnabled: false`.

## Design Principles

1. **Transparent fallback** — the client offers its keys through a single
   `gossh.PublicKeys` auth method whose signers are ordered so the local key is
   tried first; if the fast key is rejected, the SSH library continues with the
   next signer (agent / YubiKey) without any user interaction.
2. **Server keys are ephemeral** — the in-memory store is lost on server
   restart. No file I/O, no persistence.
3. **Trust chain preserved** — an auth-key can only be registered over a session
   that was already authenticated via the normal (YubiKey) path.
4. **Minimal protocol addition** — a single `AUTHKEY <base64-pubkey>` command
   sent over the existing SSH session text protocol.

## How It Works

```
┌─────────────────────────────────────────────────────────┐
│ DTail Client                                            │
│                                                         │
│  One gossh.PublicKeys auth method, signers tried in     │
│  order:                                                 │
│    1. Auth-key path (default ~/.ssh/id_rsa)  ← FAST     │
│    2. SSH agent (YubiKey)                               │
│    3. Config fallback paths, further default key paths  │
│                                                         │
│  After the session is established (and before the        │
│  DTail commands are dispatched):                        │
│    → reads <authKeyPath>.pub                            │
│    → sends AUTHKEY <base64-pubkey> to server            │
└────────────────────────┬────────────────────────────────┘
                         │ SSH
┌────────────────────────▼────────────────────────────────┐
│ DTail Server (dserver)                                  │
│                                                         │
│  PublicKeyCallback:                                    │
│    1. Check in-memory authkeystore  ← FAST (only when   │
│       AuthKeyEnabled is set)                            │
│    2. Check authorized_keys        ← existing path     │
│                                                         │
│  AUTHKEY command handler (only inside an authenticated  │
│  session):                                              │
│    → authkey.Store.Add(user, pubkey)                    │
│    → responds AUTHKEY OK / AUTHKEY ERR <reason>         │
│                                                         │
│  authkey.Store (in-memory only):                        │
│    map[username] → []PublicKey (with TTL, max per user)│
└─────────────────────────────────────────────────────────┘
```

### First connection (slow path — YubiKey)

1. The client collects signers: the key at `--auth-key-path` (default
   `~/.ssh/id_rsa`) first, then the SSH agent, then configured fallback paths and
   the remaining default key paths, all in one `gossh.PublicKeys` auth method.
2. SSH handshake begins; the server's `PublicKeyCallback` is called with the
   local key.
3. Server checks the in-memory auth-key store → not found.
4. Server checks `authorized_keys` (or the per-user authorized-keys cache) →
   not found (this key is not in there).
5. Server rejects the key; the SSH library tries the next signer: the SSH agent
   (YubiKey).
6. The YubiKey signs the challenge; the server finds the YubiKey pubkey in
   `authorized_keys` → auth succeeds.
7. Once the session is established — and before any DTail commands are
   dispatched — the client reads `<authKeyPath>.pub` and sends
   `AUTHKEY <base64-pubkey>`.
8. The server's handler parses the command and calls
   `authkey.Store.Add(user, pubkey)`; it answers `AUTHKEY OK`.

The registration is sent unconditionally (unless the feature is disabled);
re-registering an already-known key is idempotent — the server drops the old
entry and re-adds the key with a fresh TTL. If the `.pub` file is missing or
unreadable, the client silently skips the registration and logs the reason at
Debug level only.

### Subsequent connections (fast path — no YubiKey)

1. The client offers the same signers, local key first.
2. The server's `PublicKeyCallback` checks the in-memory auth-key store →
   **found** → auth succeeds immediately.
3. No YubiKey touch needed; the session is established instantly.

### Fallback (server restarted, key expired)

1. The client offers the local key → the server's auth-key store is empty or the
   entry expired → the key is rejected.
2. The client's auth method continues with the next signer → YubiKey auth
   succeeds.
3. The client re-registers the local pubkey with the `AUTHKEY` command, so the
   next connection is fast again.

## Components

### Server: in-memory auth-key store (`internal/authkey/authkeystore.go`)

`authkey.Store` is a thread-safe, per-user cache of SSH public keys, held in a
`map[string][]authKeyEntry` guarded by a mutex (one entry per registered key,
storing the `gossh.PublicKey` and its registration time). Each `dserver`
instance owns exactly one store; there is no package-level singleton and no
disk persistence.

* `Add(user, pubKey)` — removes any duplicate entry for the same key and
  re-adds it with a fresh TTL, so re-registration is idempotent. When the user
  is at the per-user key limit, the oldest entries are dropped.
* `Has(user, pubKey)` — returns true only for a non-expired key.
* `Remove(user, pubKey)` — deletes a single key.
* Expiry is lazy: `Add`, `Has` and `Remove` prune a user's expired entries
  before doing their work; there is no background reaper.
* Bounds: TTL defaults to 24h (`AuthKeyTTLSeconds`, 86400) and at most 5 keys
  per user are kept (`AuthKeyMaxPerUser`). Both are configurable.

### Server: PublicKeyCallback (`internal/ssh/server/publickeycallback.go`)

`NewPublicKeyCallback` wires the store into the SSH handshake. When
`AuthKeyEnabled` is set, the callback checks the in-memory store **first**; on a
hit the connection is authorized immediately ("Authorized by in-memory auth
key store"). On a miss it falls through to the existing file-based logic — the
configured `AuthorizedKeysPath`, the per-user authorized-keys cache, or
`~/.ssh/authorized_keys` — with no behaviour change. When the feature is
disabled, only the `authorized_keys` path is consulted.

### Server: AUTHKEY command handler (`internal/handlers/serverhandler.go`)

The `AUTHKEY <base64-pubkey>` command is accepted only within an already
authenticated session. The handler rejects it with `AUTHKEY ERR <reason>` when:

* the feature is disabled on the server (`feature disabled`),
* the session user is a password-only user (`unsupported user`),
* no public key was supplied (`missing public key`),
* the base64 payload does not decode (`invalid base64`),
* the decoded blob is not an SSH public key (`invalid public key`), or
* the server has no key store wired up (`internal key store unavailable`).

Otherwise the key is added to the store and the client receives `AUTHKEY OK`.

### Client: auth method ordering (`internal/ssh/client/authmethods.go`)

The client uses a **single** `gossh.AuthMethod`, `gossh.PublicKeys(signers...)`,
whose signers are tried in this order:

1. the explicit auth-key path (`--auth-key-path`, defaulting to
   `~/.ssh/id_rsa`),
2. all SSH agent keys (a YubiKey is typically exposed here),
3. additional configured fallback key paths,
4. the further default key paths `~/.ssh/id_dsa`, `~/.ssh/id_ecdsa` and
   `~/.ssh/id_ed25519`.

Duplicate keys are de-duplicated. Because all signers live in one auth method,
Go's SSH client transparently moves to the next signer when one is rejected —
that is the transparent fallback.

### Client: auth-key registration (`internal/clients/connectors/serverconnection.go`)

Once the SSH session is established — and before the DTail commands are
dispatched — every connection registers the auth key, unless the feature is
disabled. The client reads `<authKeyPath>.pub`, takes the base64 key blob from
its first non-comment line, and sends `AUTHKEY <base64>`. The server's reply is
an ordinary session message; registration is best-effort. When the `.pub` file
is missing or unreadable, or the SSH private key path could not be resolved at
all (see below), the client logs the skip at Debug level and continues.

### Configuration and flags

Server config (`dtail.json`, `internal/config/server.go`):

* `AuthKeyEnabled` (bool, default `true`) — enable in-memory auth-key
  registration and fast reconnect.
* `AuthKeyTTLSeconds` (int, default `86400` = 24h) — TTL of a cached key.
* `AuthKeyMaxPerUser` (int, default `5`) — maximum cached keys per user.

```json
{
  "Server": {
    "AuthKeyEnabled": true,
    "AuthKeyTTLSeconds": 86400,
    "AuthKeyMaxPerUser": 5
  }
}
```

Client flags and config (`internal/cli/authkeyflags.go`,
`internal/cli/client.go`, `internal/config/client.go`):

* `--auth-key-path` (default `~/.ssh/id_rsa`) — path to the local private key
  tried first and whose `.pub` counterpart is registered.
* `--key` — deprecated alias for `--auth-key-path`; a warning is printed when
  both are given (`--auth-key-path` wins).
* `--no-auth-key` (or `Client.AuthKeyDisable: true` in the config file) —
  disable the feature entirely.
* `Client.AuthKeyPath` — the config-file equivalent of `--auth-key-path`.

Environment variables (`internal/config/initializer.go`), in order of
precedence (highest first): the CLI flag, then `DTAIL_AUTH_KEY_PATH`, then the
legacy `DTAIL_SSH_PRIVATE_KEYFILE_PATH` (used only when `DTAIL_AUTH_KEY_PATH`
is unset), then the config-file value. When no path can be determined at all —
for example because `$HOME` is empty and nothing else was configured — the
feature is disabled with a warning telling the user to set
`DTAIL_AUTH_KEY_PATH` explicitly.

`dtailhealth` accepts `--no-auth-key` but has no `--auth-key-path` flag
(`cmd/dtailhealth/main.go`).

## Security Considerations

- **No server-side disk persistence** — keys exist only in memory, lost on restart.
- **Trust chain** — auth-keys can only be registered over an already-authenticated
  session. An attacker cannot register a key without first proving identity.
- **Delegated credential and revocation window** — an auth-key is a temporary,
  per-user credential accepted by every `dserver` instance that has cached it.
  Possession of its private key permits access as that user until the entry
  expires (default 24h) or the server restarts. Removing or revoking the normal
  key that originally authenticated the session does **not** remove an already
  cached auth-key.
- **Per-user limits** — max 5 keys per user prevents memory exhaustion.
- **Protect the local private key** — use restrictive file permissions (normally
  `0600`). The auth-key need not itself be in `authorized_keys`; if it is
  compromised, it grants access only to the `dserver` instances that still
  cache it, but it can still be sufficient to read everything that user may
  read there.
- **Protocol and operational scope** — `AUTHKEY` is accepted only inside an
  authenticated session, so it does not allow unauthenticated registration.
  It intentionally expands the set of keys a server accepts for the cache
  lifetime, much like temporarily adding the key to that user's
  `authorized_keys`. Choose a TTL that fits the desired revocation window, or
  disable the feature where that trade-off is unacceptable.
