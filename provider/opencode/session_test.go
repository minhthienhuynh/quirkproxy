package opencode

import (
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
