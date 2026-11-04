package agent

// NewWithHistory returns a fresh agent sharing the conversation history
// but using the latest per-turn options (notably OnDelta). In chat mode
// each user turn streams through the current UI hook.
func NewWithHistory(prev *Agent, opts Options) *Agent {
	a := New(opts)
	a.messages = prev.messages
	a.tokens = prev.tokens
	a.turn = prev.turn
	return a
}
