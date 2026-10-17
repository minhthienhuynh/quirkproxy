package opencode

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsNativeSessionID(t *testing.T) {
	valid := "ses_019982438f931acdf074019e12"
	if !isNativeSessionID(valid) {
		t.Fatalf("expected %q to be recognized as native session id", valid)
	}

	invalidCases := []string{
		"",
		"ses_",
		"ses_019982438f9",                      // too short
		"msg_019982438f931acdf074019e12",       // wrong prefix
		"ses_019982438f931acdf074019e12345678", // too long
		"ses_019982438f9z1acdf074019e12",       // 'z' in hex part
	}
	for _, tc := range invalidCases {
		if isNativeSessionID(tc) {
			t.Fatalf("expected %q to be recognized as NOT native session id", tc)
		}
	}
}

func TestTranslateSessionID(t *testing.T) {
	native := "ses_019982438f931acdf074019e12"
	if got := translateSessionID(native); got != native {
		t.Fatalf("native session %q was modified to %q", native, got)
	}

	hint1 := "user_conversation_uuid_12345"
	id1 := translateSessionID(hint1)
	if !isNativeSessionID(id1) {
		t.Fatalf("translated id %q is not valid native session id", id1)
	}

	// Deterministic: same hint produces same id
	id2 := translateSessionID(hint1)
	if id1 != id2 {
		t.Fatalf("translation is not deterministic: %q != %q", id1, id2)
	}

	// Distinct hints produce distinct ids
	hint2 := "user_conversation_uuid_67890"
	id3 := translateSessionID(hint2)
	if id1 == id3 {
		t.Fatalf("distinct hints produced same session id: %q == %q", id1, id3)
	}
}

func TestSessionManager(t *testing.T) {
	sm := newSessionManager(100*time.Millisecond, 2)

	// Explicit hint wins
	native := "ses_019982438f931acdf074019e12"
	if got := sm.Resolve(native, "client-A"); got != native {
		t.Fatalf("hint did not win: got %q, want %q", got, native)
	}

	// Affinity cache: same clientID gets same session ID within TTL
	sesA1 := sm.Resolve("", "client-A")
	if !isNativeSessionID(sesA1) {
		t.Fatalf("invalid session id: %q", sesA1)
	}
	sesA2 := sm.Resolve("", "client-A")
	if sesA1 != sesA2 {
		t.Fatalf("session affinity failed: %q != %q", sesA1, sesA2)
	}

	// Different client gets different session ID
	sesB1 := sm.Resolve("", "client-B")
	if sesA1 == sesB1 {
		t.Fatalf("different clients got same session ID: %q", sesA1)
	}

	// Eviction when maxSize is exceeded (client-C evicts oldest, but let's check capacity)
	sesC1 := sm.Resolve("", "client-C")
	if !isNativeSessionID(sesC1) {
		t.Fatalf("invalid session id for client C: %q", sesC1)
	}

	// TTL expiration
	time.Sleep(120 * time.Millisecond)
	sesA3 := sm.Resolve("", "client-A")
	if sesA1 == sesA3 {
		t.Fatalf("session did not expire after TTL: %q == %q", sesA1, sesA3)
	}

	// Explicit reset request: evicts cache and generates new session
	sesA4 := sm.Resolve("new", "client-A")
	if sesA4 == sesA3 {
		t.Fatalf("explicit reset did not generate new session: %q == %q", sesA4, sesA3)
	}
	// Subsequent call reuses the newly minted session
	sesA5 := sm.Resolve("", "client-A")
	if sesA5 != sesA4 {
		t.Fatalf("subsequent call did not reuse reset session: %q != %q", sesA5, sesA4)
	}
}

func TestSessionManagerConcurrent(t *testing.T) {
	sm := newSessionManager(time.Minute, 100)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := "client-concurrent"
			if id%2 == 0 {
				client = "client-other"
			}
			for j := 0; j < 50; j++ {
				_ = sm.Resolve("", client)
				_ = sm.Resolve("custom-hint", client)
			}
		}(i)
	}
	wg.Wait()
}

func TestHermesSessionLifecycle(t *testing.T) {
	sm := newSessionManager(30*time.Minute, 1000)

	// Case 1: In-session turns (Hermes keeps session_id across turns)
	hermesSessionID := "20261017_040311_4a0047d7"
	turn1Ses := sm.Resolve(hermesSessionID, "hermes-agent")
	turn2Ses := sm.Resolve(hermesSessionID, "hermes-agent")

	if !isNativeSessionID(turn1Ses) {
		t.Fatalf("turn 1 session is not native format: %q", turn1Ses)
	}
	if turn1Ses != turn2Ses {
		t.Fatalf("in-session turns must produce identical upstream session IDs: %q != %q", turn1Ses, turn2Ses)
	}

	// Case 2: New session (e.g. user types /new or session rotates)
	newHermesSessionID := "20261017_052000_9b8a7c6d"
	newSes := sm.Resolve(newHermesSessionID, "hermes-agent")

	if !isNativeSessionID(newSes) {
		t.Fatalf("new session is not native format: %q", newSes)
	}
	if newSes == turn1Ses {
		t.Fatalf("new session must produce a different upstream session ID from old session: %q == %q", newSes, turn1Ses)
	}

	// Case 3: In-session turns inside the new session remain stable
	newSesTurn2 := sm.Resolve(newHermesSessionID, "hermes-agent")
	if newSes != newSesTurn2 {
		t.Fatalf("turns within new session must remain stable: %q != %q", newSes, newSesTurn2)
	}
}

// decodeSessionCounter extracts the counter packed into a ses_ id's first 12
// hex chars (the payload is the bitwise inverse of ms*4096+counter).
func decodeSessionCounter(t *testing.T, id string) uint32 {
	t.Helper()
	const prefix = "ses_"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+12+14 {
		t.Fatalf("decodeSessionCounter: not a ses_ id: %q", id)
	}
	raw, err := hex.DecodeString(id[len(prefix) : len(prefix)+12])
	if err != nil {
		t.Fatalf("decodeSessionCounter: hex decode %q: %v", id, err)
	}
	var val uint64
	for _, b := range raw {
		val = val<<8 | uint64(b)
	}
	return uint32((^val) % 4096)
}

// TestNewSessionIDCounterStaysWithinGateBound is the regression for the 403
// storm: the old monotonic counter left the gate's accepted range after 64
// mints in one process lifetime. The genuine client resets its counter every
// millisecond, so minting at a realistic cadence must keep the packed counter
// < 64 for the whole run.
func TestNewSessionIDCounterStaysWithinGateBound(t *testing.T) {
	for i := 1; i <= 200; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if got := decodeSessionCounter(t, id); got >= 64 {
			t.Fatalf("mint %d: counter = %d, want < 64 (gate bound)", i, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestNewSessionIDCounterResetsPerMillisecond mirrors the genuine client's
// SMn: within one millisecond the counter increments; when the timestamp
// advances it resets to 1, so the packed counter never rots with uptime.
func TestNewSessionIDCounterResetsPerMillisecond(t *testing.T) {
	mint := func(ms int64) uint32 {
		t.Helper()
		id, err := newSessionIDAt(ms)
		if err != nil {
			t.Fatalf("mint at %d: %v", ms, err)
		}
		return decodeSessionCounter(t, id)
	}
	const ms = int64(1734567890123)
	if got := mint(ms); got != 1 {
		t.Fatalf("first mint in a fresh millisecond: counter = %d, want 1", got)
	}
	if got := mint(ms); got != 2 {
		t.Fatalf("second mint in the same millisecond: counter = %d, want 2", got)
	}
	if got := mint(ms + 1); got != 1 {
		t.Fatalf("first mint after the millisecond advanced: counter = %d, want reset to 1", got)
	}
	if got := mint(ms + 1); got != 2 {
		t.Fatalf("second mint in the new millisecond: counter = %d, want 2", got)
	}
}
