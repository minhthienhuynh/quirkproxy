package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"time"
)

var nativeSessionPat = regexp.MustCompile(`^ses_[0-9a-fA-F]{12}[0-9A-Za-z]{14}$`)

// isNativeSessionID reports whether s matches the OpenCode v2 session format.
func isNativeSessionID(s string) bool {
	return nativeSessionPat.MatchString(s)
}

// translateSessionID maps an arbitrary session or conversation hint into a valid
// OpenCode v2 session ID (ses_<12hex><14base62>). If the hint is already in native
// format, it is returned untouched. Otherwise, it is deterministically hashed
// with a pinned counter (=1) so repeated calls with the same hint produce the
// exact same session ID without triggering upstream counter rejections.
func translateSessionID(hint string) string {
	trimmed := strings.TrimSpace(hint)
	if isNativeSessionID(trimmed) {
		return trimmed
	}
	h := sha256.Sum256([]byte("opencode\x00" + trimmed))
	timeVal := (uint64(h[0])<<32 | uint64(h[1])<<24 | uint64(h[2])<<16 | uint64(h[3])<<8 | uint64(h[4])) & ((1 << 36) - 1)
	val := ^(timeVal*4096 + 1)
	timeBytes := make([]byte, 6)
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((val >> (40 - 8*uint(i))) & 0xff)
	}
	timeHex := hex.EncodeToString(timeBytes)
	b62 := make([]byte, 14)
	for i := 0; i < 14; i++ {
		b62[i] = b62Chars[int(h[6+i])%len(b62Chars)]
	}
	return "ses_" + timeHex + string(b62)
}

type sessionEntry struct {
	sessionID string
	lastUsed  time.Time
}

// SessionManager manages affinity sessions per client identity to prevent
// burning free-tier quota by generating fresh sessions on every request.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
	ttl      time.Duration
	maxSize  int
}

const (
	defaultSessionTTL  = 30 * time.Minute
	defaultMaxSessions = 1000
)

// newSessionManager initializes a SessionManager with bounds.
func newSessionManager(ttl time.Duration, maxSize int) *SessionManager {
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	if maxSize <= 0 {
		maxSize = defaultMaxSessions
	}
	return &SessionManager{
		sessions: make(map[string]sessionEntry),
		ttl:      ttl,
		maxSize:  maxSize,
	}
}

// Resolve returns the appropriate session ID.
//  1. If hint is non-empty (and not a reset directive), it is translated
//     (or passed through if native) without acquiring the mutex.
//  2. If hint == "new" or hint == "reset", the client's cached session is evicted
//     and a fresh session ID is minted and stored.
//  3. Otherwise, a stable cached session is reused for clientID within ttl.
//  4. If no cached session exists or it expired, a fresh session ID is minted.
func (sm *SessionManager) Resolve(hint, clientID string) string {
	if hint != "" && hint != "new" && hint != "reset" {
		return translateSessionID(hint)
	}

	if clientID == "" {
		clientID = "default"
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()

	// Explicit reset request: evict cache and mint fresh session
	if hint == "new" || hint == "reset" {
		delete(sm.sessions, clientID)
		newID, err := newSessionID()
		if err != nil {
			return translateSessionID(now.String())
		}
		sm.sessions[clientID] = sessionEntry{
			sessionID: newID,
			lastUsed:  now,
		}
		return newID
	}

	if entry, ok := sm.sessions[clientID]; ok {
		if now.Sub(entry.lastUsed) < sm.ttl {
			entry.lastUsed = now
			sm.sessions[clientID] = entry
			return entry.sessionID
		}
	}

	newID, err := newSessionID()
	if err != nil {
		return translateSessionID(clientID)
	}

	if len(sm.sessions) >= sm.maxSize {
		for k, v := range sm.sessions {
			if now.Sub(v.lastUsed) >= sm.ttl {
				delete(sm.sessions, k)
			}
		}
		if len(sm.sessions) >= sm.maxSize {
			var oldestKey string
			var oldestTime time.Time
			first := true
			for k, v := range sm.sessions {
				if first || v.lastUsed.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastUsed
					first = false
				}
			}
			delete(sm.sessions, oldestKey)
		}
	}

	sm.sessions[clientID] = sessionEntry{
		sessionID: newID,
		lastUsed:  now,
	}
	return newID
}
