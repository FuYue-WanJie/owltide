package agent

// P2b-M6: compaction trigger at turn boundaries. The hook runs once per
// completed turn (never mid-turn): it may shrink a.messages by folding
// old turns into summaries, which is the ONLY point the request prefix
// changes. The sessionlog stays untouched (single source of truth).

import (
	"github.com/FuYue-WanJie/owltide/core/compaction"
	"github.com/FuYue-WanJie/owltide/core/model"
)

// CompactOptions configures the agent-side trigger.
type CompactOptions struct {
	// ContextTokens is the resolved model context window (the only
	// denominator).
	ContextTokens int
	// Ratio is budget.compact_at (0 disables).
	Ratio float64
	// Persist stores a record; nil keeps records in memory only.
	Persist func(compaction.Record) error
	// OnFold reports each applied fold (decisions event sink).
	OnFold func(compaction.Result)
}

// maybeCompact runs one compaction pass at the called turn boundary.
// It returns true when the in-memory history changed.
func (a *Agent) maybeCompact() bool {
	if a.opts.Compact == nil || a.opts.Compact.ContextTokens <= 0 {
		return false
	}
	res := compaction.Compact(a.messages, compaction.Options{
		ContextTokens: a.opts.Compact.ContextTokens,
		Ratio:         a.opts.Compact.Ratio,
	})
	if !res.Changed {
		return false
	}
	a.messages = res.Messages
	if a.opts.Compact.Persist != nil {
		for _, r := range res.NewRecords {
			_ = a.opts.Compact.Persist(r)
		}
	}
	if a.opts.Compact.OnFold != nil {
		a.opts.Compact.OnFold(res)
	}
	// The prefix changed exactly here (by design); recompute so a
	// later no-op turn keeps the new hash.
	a.prefixHash = a.computePrefixHash()
	return true
}

// fullHistory returns the un-compacted history (for context_expand /
// context_search): the compacted array is not enough, so callers use
// the sessionlog rebuild.
func (a *Agent) fullHistory() []model.Message { return a.messages }
