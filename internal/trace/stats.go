package trace

import "codedeep/internal/agent"

// Summarize aggregates the usage, tool, and cost totals of an event stream.
// The terminal done event wins because its totals include subagent usage
// exactly once.
func Summarize(events []agent.Event) (modelCalls, toolCalls, tokens int, cost float64) {
	var summedTokens int
	var summedCost float64
	var done *agent.Event
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case agent.EventUsage:
			modelCalls++
			if ev.Usage != nil {
				summedTokens += ev.Usage.TotalTokens
			}
			summedCost += ev.CostUSD
		case agent.EventToolStart:
			toolCalls++
		case agent.EventDone:
			done = &events[i]
		}
	}
	tokens, cost = summedTokens, summedCost
	if done != nil {
		if done.TotalUsage != nil {
			tokens = done.TotalUsage.TotalTokens
		}
		cost = done.TotalCost
	}
	return modelCalls, toolCalls, tokens, cost
}
