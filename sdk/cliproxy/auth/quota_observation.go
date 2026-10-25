package auth

import (
	"net/http"
	"time"
)

// ObserveQuotaHeaders publishes an authenticated usage probe to routing without
// persisting credentials or replacing a newer execution observation.
func (m *Manager) ObserveQuotaHeaders(id string, epoch uint64, headers http.Header, observedAt time.Time) {
	if m == nil || observedAt.IsZero() {
		return
	}
	m.mu.Lock()
	a := m.auths[id]
	if a == nil || a.RegistrationEpoch != epoch || !a.Quota.ObservedAt.Before(observedAt) {
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
}
