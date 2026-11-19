package usagehistory

import "sort"

// Breakdown reports three views of the same usage. Every view includes usage
// for which a project or an account was not recorded.
type Breakdown struct {
	Projects        []*AttributionEntry `json:"projects"`
	Accounts        []*AttributionEntry `json:"accounts"`
	ProjectAccounts []*AttributionEntry `json:"project_accounts"`
}

type AttributionEntry struct {
	Project   string `json:"project,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	UsageEntry
}

type attributionKey struct{ project, provider, account string }

type attributionGroup map[attributionKey]*AttributionEntry

func (g attributionGroup) add(key attributionKey, counters Counters, cost CostBreakdown, priced bool) {
	entry := g[key]
	if entry == nil {
		entry = &AttributionEntry{Project: key.project, AuthIndex: key.account, UsageEntry: UsageEntry{Provider: key.provider}}
		g[key] = entry
	}
	entry.add(counters, cost, priced)
}

func (g attributionGroup) finish() []*AttributionEntry {
	entries := make([]*AttributionEntry, 0, len(g))
	for _, entry := range g {
		entry.finish()
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.AuthIndex < b.AuthIndex
	})
	return entries
}

type attributionTotals struct {
	projects, accounts, pairs attributionGroup
}

func newAttributionTotals() *attributionTotals {
	return &attributionTotals{make(attributionGroup), make(attributionGroup), make(attributionGroup)}
}

func (a *attributionTotals) add(row Row, cost CostBreakdown, priced bool) {
	account := row.AuthIndex
	if account == importedAuthIndex {
		account = ""
	}
	a.projects.add(attributionKey{project: row.Project}, row.Counters, cost, priced)
	a.accounts.add(attributionKey{provider: row.Provider, account: account}, row.Counters, cost, priced)
	a.pairs.add(attributionKey{project: row.Project, provider: row.Provider, account: account}, row.Counters, cost, priced)
}

func (a *attributionTotals) finish() *Breakdown {
	return &Breakdown{Projects: a.projects.finish(), Accounts: a.accounts.finish(), ProjectAccounts: a.pairs.finish()}
}
