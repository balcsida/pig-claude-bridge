package models

import "encoding/json"

// pendingJSON holds models Claude Code ships before pi-ai's catalog does, in
// pi-ai's entry shape. Drop an entry once a regenerated catalog carries its id;
// TestPendingEntriesLeaveWithTheCatalog fails until that happens.
//
// claude-fable-5-2: Claude Code 2.1.321's catalog (1M native context, 128K
// output, pricing tier_8_40_cache_read_0_20), capabilities copied from 5.1.
const pendingJSON = `[
{"api":"anthropic-messages","baseUrl":"https://api.anthropic.com","compat":{"forceAdaptiveThinking":true,"supportsMidConvoEffort":true,"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true,"supportsStrictTools":true},"contextWindow":1000000,"cost":{"cacheRead":0.2,"cacheWrite":10,"input":8,"output":40},"id":"claude-fable-5-2","input":["text","image"],"inputLimits":{"images":{"maxPerRequest":600,"resize":{"jpegQuality":80,"maxBytes":4718592,"maxHeight":2000,"maxWidth":2000}},"maxRequestBytes":33554432},"maxTokens":128000,"name":"Claude Fable 5.2","promptCache":{"long":3600,"short":300},"provider":"anthropic","reasoning":true,"thinkingLevelMap":{"max":"max","off":null,"xhigh":"xhigh"},"type":"chat"}
]`

func pendingEntries() []map[string]any {
	var entries []map[string]any
	if err := json.Unmarshal([]byte(pendingJSON), &entries); err != nil {
		panic("models: pending catalog is invalid: " + err.Error())
	}
	return entries
}
