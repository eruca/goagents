package main

import (
	"context"
	"github.com/eruca/goagents/goagent/ports"
)

// Knowledge-specific instructions are trusted caller configuration; imported
// evidence stays inside the existing extractor's untrusted JSON text boundary.
type knowledgeClient struct{ client ports.LLMClient }

func (c knowledgeClient) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	messages := append([]ports.ChatMessage(nil), req.Messages...)
	messages = append([]ports.ChatMessage{{Role: "system", Content: `Extract at most 8 short knowledge units. Preserve attribution, conditions and uncertainty. A paper's claim is not an approved personal decision. Do not invent facts or decisions. Return only {"candidates":[{"kind":"fact","key":"temporary-key","content":"The source reports ...","importance":50,"confidence":0.5}]}. Allowed kind values: fact, decision, constraint, lesson. Each content is at most 2400 Unicode characters. Omit validity dates unless the source explicitly supports them. Use an empty candidates array if no useful knowledge is supported. Do not include scope, sources, status, tools or other fields. Keys will be assigned by the caller.`}}, messages...)
	req.Messages = messages
	return c.client.Chat(ctx, req)
}
