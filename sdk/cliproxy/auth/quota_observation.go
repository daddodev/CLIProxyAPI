package auth

import (
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageupdates"
)

// ObserveQuotaHeaders publishes an authenticated usage observation to routing without
// persisting credentials or replacing a newer execution observation.
func (m *Manager) ObserveQuotaHeaders(id string, epoch uint64, headers http.Header, observedAt time.Time) {
	m.observeQuotaHeaders(id, epoch, 0, headers, observedAt)
}

func (m *Manager) observeQuotaHeaders(id string, epoch, version uint64, headers http.Header, observedAt time.Time) {
	if m == nil || observedAt.IsZero() {
		return
	}
	m.mu.Lock()
	a := m.auths[id]
	if a == nil || a.RegistrationEpoch != epoch || (version != 0 && a.CredentialVersion != version) || !a.Quota.ObservedAt.Before(observedAt) {
		m.mu.Unlock()
		return
	}
	changed := a.Quota.ObserveResponseHeadersForProvider(a.Provider, headers, observedAt)
	var snapshot *Auth
	if changed {
		a.Generation++
		snapshot = a.Clone()
	}
	m.mu.Unlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if changed {
		usageupdates.Notify()
	}
}
