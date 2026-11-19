package authkey

import (
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestAuthKeyStoreAddHasRemove(t *testing.T) {
	store := New(time.Hour, 5)
	key := testPublicKey(t, 1)

	if store.Has("alice", key) {
		t.Fatalf("Store should not contain key before add")
	}

	store.Add("alice", key)
	if !store.Has("alice", key) {
		t.Fatalf("Store should contain key after add")
	}

	store.Remove("alice", key)
	if store.Has("alice", key) {
		t.Fatalf("Store should not contain key after remove")
	}
}

func TestAuthKeyStoreHasExpiresKeysLazily(t *testing.T) {
	now := time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)
	store := newAuthKeyStoreWithClock(10*time.Second, 5, func() time.Time { return now })
	key := testPublicKey(t, 2)

	store.Add("alice", key)
	if !store.Has("alice", key) {
		t.Fatalf("Store should contain fresh key")
	}

	now = now.Add(11 * time.Second)
	if store.Has("alice", key) {
		t.Fatalf("Store should expire key when ttl is exceeded")
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.keysByUser["alice"]) != 0 {
		t.Fatalf("Expired entries should be removed on Has call")
	}
}

func TestAuthKeyStoreEnforcesPerUserKeyLimit(t *testing.T) {
	now := time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)
	store := newAuthKeyStoreWithClock(time.Hour, 2, func() time.Time { return now })

	keyOne := testPublicKey(t, 3)
	keyTwo := testPublicKey(t, 4)
	keyThree := testPublicKey(t, 5)

	store.Add("alice", keyOne)
	now = now.Add(1 * time.Second)
	store.Add("alice", keyTwo)
	now = now.Add(1 * time.Second)
	store.Add("alice", keyThree)

	if store.Has("alice", keyOne) {
		t.Fatalf("Oldest key should be evicted once max key limit is reached")
	}
	if !store.Has("alice", keyTwo) {
		t.Fatalf("Second key should remain in store")
	}
	if !store.Has("alice", keyThree) {
		t.Fatalf("Newest key should remain in store")
	}
}

func TestAuthKeyStoreAddRefreshesExistingKey(t *testing.T) {
	now := time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)
	store := newAuthKeyStoreWithClock(10*time.Second, 5, func() time.Time { return now })
	key := testPublicKey(t, 6)

	store.Add("alice", key)
	now = now.Add(9 * time.Second)
	store.Add("alice", key)

	now = now.Add(5 * time.Second)
	if !store.Has("alice", key) {
		t.Fatalf("Key should stay valid after it is refreshed")
	}

	now = now.Add(6 * time.Second)
	if store.Has("alice", key) {
		t.Fatalf("Refreshed key should expire once ttl is exceeded from latest add")
	}
}

func TestAuthKeyStoreUserIsolation(t *testing.T) {
	store := New(time.Hour, 5)
	key := testPublicKey(t, 7)

	store.Add("alice", key)
	if store.Has("bob", key) {
		t.Fatalf("Key lookup must be isolated by user")
	}
}

func TestAuthKeyStoreIgnoresInvalidInput(t *testing.T) {
	store := New(time.Hour, 5)
	key := testPublicKey(t, 8)

	store.Add("", key)
	store.Add("alice", nil)
	store.Remove("", key)
	store.Remove("alice", nil)

	if store.Has("", key) {
		t.Fatalf("Empty user should not match")
	}
	if store.Has("alice", nil) {
		t.Fatalf("Nil key should not match")
	}
}

func TestAuthKeyStoreConcurrentAccess(t *testing.T) {
	store := New(time.Hour, 5)
	users := []string{"alice", "bob", "carol"}
	keys := []gossh.PublicKey{
		testPublicKey(t, 11),
		testPublicKey(t, 12),
		testPublicKey(t, 13),
		testPublicKey(t, 14),
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			user := users[workerID%len(users)]
			for i := 0; i < 200; i++ {
				key := keys[(workerID+i)%len(keys)]
				store.Add(user, key)
				_ = store.Has(user, key)
				if i%3 == 0 {
					store.Remove(user, key)
				}
			}
		}(worker)
	}
	wg.Wait()

	store.mu.RLock()
	defer store.mu.RUnlock()
	for user, userEntries := range store.keysByUser {
		if len(userEntries) > store.maxKeysPerUser {
			t.Fatalf("User %s exceeded max key limit: %d", user, len(userEntries))
		}
	}
}

func testPublicKey(t *testing.T, seedByte byte) gossh.PublicKey {
	t.Helper()

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}

	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, err := gossh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("Unable to build ssh public key: %s", err.Error())
	}

	return publicKey
}
