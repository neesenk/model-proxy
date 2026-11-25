package runtime

import (
	"strings"
	"unicode/utf8"
)

// credentialDenialErrMaxRunes bounds the evidence kept per denial. It is the
// upstream's own error message, shown as the credential banner's tooltip —
// the full body (when there is one) lives in the request-log attempt row.
const credentialDenialErrMaxRunes = 200

// RecordCredentialDenial records a credential failure the forwarding path
// PROVED for one account: an upstream 401 after refresh, an
// account-disablement code, a lapsed plan subscription (the account
// attributions of docs/architecture/routing-and-failure.md). The quota poll
// cannot see these for providers whose Quota() never contacts the upstream
// (step-plan, qwen-plan, mimo, static), so without this record a dead account
// only ever shows as failover legs while its siblings carry the traffic.
//
// Lifecycle: the record lives until the account's next successful request
// (RecordSuccess) or until the account leaves the config (ReplaceGeneration
// prunes to the live key set — unlike health this is an observed fact about
// the credential, not config-coupled scheduling state, so a reload must not
// erase it). unfreeze keeps it too: it asks for a retry, it is not a
// recovery. Memory-only, like the tracker's sync-error records. A repeated
// denial refreshes the class and evidence but keeps Since at the episode's
// first observation, so the banner can say how long the account has been
// dead. class uses the CredentialErrClass vocabulary; an empty class or name
// records nothing.
func (m *Manager) RecordCredentialDenial(name, class, err string, generation uint64) {
	if name == "" || class == "" {
		return
	}
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return
	}
	denial := CredentialDenial{Class: class, Err: boundCredentialDenialErr(err), Since: now}
	if prev, ok := m.denials[name]; ok {
		denial.Since = prev.Since
	}
	m.denials[name] = denial
}

// boundCredentialDenialErr keeps the first line of the upstream evidence,
// trimmed and capped at credentialDenialErrMaxRunes so a multi-line error
// page or a long JSON body cannot bloat the status payload. Runes (not
// bytes) are counted so the cap never splits a multi-byte character.
func boundCredentialDenialErr(err string) string {
	line := err
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > credentialDenialErrMaxRunes {
		line = string([]rune(line)[:credentialDenialErrMaxRunes])
	}
	return line
}
