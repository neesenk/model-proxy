package runtime

import "model-proxy/internal/provider"

func (m *Manager) MergeQuotas(snapshots map[string]*provider.QuotaSnapshot, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	for name, snapshot := range snapshots {
		m.quotas[name] = cloneQuota(snapshot)
	}
	return true
}

func (m *Manager) SetQuota(name string, snapshot *provider.QuotaSnapshot, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.quotas[name] = cloneQuota(snapshot)
	return true
}

func (m *Manager) Quota(name string) *provider.QuotaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneQuota(m.quotas[name])
}

func (m *Manager) Quotas() map[string]*provider.QuotaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneQuotas(m.quotas)
}

func (m *Manager) ClearQuotas(generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.quotas = make(map[string]*provider.QuotaSnapshot)
	return true
}

func cloneQuotas(in map[string]*provider.QuotaSnapshot) map[string]*provider.QuotaSnapshot {
	out := make(map[string]*provider.QuotaSnapshot, len(in))
	for key, value := range in {
		out[key] = cloneQuota(value)
	}
	return out
}

func cloneQuota(in *provider.QuotaSnapshot) *provider.QuotaSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Notes = append([]string(nil), in.Notes...)
	out.Windows = append([]provider.QuotaWindow(nil), in.Windows...)
	for i := range out.Windows {
		out.Windows[i].Details = append([]provider.QuotaDetail(nil), in.Windows[i].Details...)
	}
	return &out
}
