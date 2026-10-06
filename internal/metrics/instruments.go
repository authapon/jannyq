package metrics

// Instruments are the counters and histograms of the bot. The zero value
// (what NewInstruments(nil) returns) does nothing, so components take an
// Instruments without caring whether metrics are on.
type Instruments struct {
	MessagesReceived *Counter   // channel, addressed
	MessagesRejected *Counter   // channel, reason
	Replies          *Counter   // channel, result
	RequestSeconds   *Histogram // channel
	LLMRequests      *Counter   // result
	LLMSeconds       *Histogram
	LLMTokens        *Counter // type
	ToolCalls        *Counter // tool, result
	Compactions      *Counter // result
	Attachments      *Counter // kind, result
	KnowledgeScans   *Counter // result
	KnowledgeChanges *Counter // change
	EmbedFailures    *Counter
	HTTPRequests     *Counter // class
	Backups          *Counter // result
	Retention        *Counter // what
}

// NewInstruments registers the bot's metrics on r (nil gives no-op instruments).
func NewInstruments(r *Registry) Instruments {
	return Instruments{
		MessagesReceived: r.Counter("jannyq_messages_received_total", "Messages received from users.", "channel", "addressed"),
		MessagesRejected: r.Counter("jannyq_messages_rejected_total", "Messages turned away before reaching the model.", "channel", "reason"),
		Replies:          r.Counter("jannyq_replies_total", "Answers to users by result (ok, empty, error).", "channel", "result"),
		RequestSeconds:   r.Histogram("jannyq_request_duration_seconds", "Time to answer a message, including tool calls.", nil, "channel"),
		LLMRequests:      r.Counter("jannyq_llm_requests_total", "Requests to the model by result (ok, error).", "result"),
		LLMSeconds:       r.Histogram("jannyq_llm_request_duration_seconds", "Duration of one model request.", nil),
		LLMTokens:        r.Counter("jannyq_llm_tokens_total", "Tokens as counted by the model server.", "type"),
		ToolCalls:        r.Counter("jannyq_tool_calls_total", "Tool calls by tool and result (ok, error).", "tool", "result"),
		Compactions:      r.Counter("jannyq_compactions_total", "Conversation compactions by result (ok, error).", "result"),
		Attachments:      r.Counter("jannyq_attachments_total", "Files sent by users by kind and result (read, refused, failed, skipped).", "kind", "result"),
		KnowledgeScans:   r.Counter("jannyq_knowledge_scans_total", "Scans of the knowledge folder by result (ok, error).", "result"),
		KnowledgeChanges: r.Counter("jannyq_knowledge_changes_total", "Changes applied to the knowledge index.", "change"),
		EmbedFailures:    r.Counter("jannyq_knowledge_embed_failures_total", "Failed embedding requests while indexing."),
		HTTPRequests:     r.Counter("jannyq_http_requests_total", "HTTP requests by status class.", "class"),
		Backups:          r.Counter("jannyq_backups_total", "Backups by result (ok, error).", "result"),
		Retention:        r.Counter("jannyq_retention_deleted_total", "Chats deleted for being idle for longer than the retention period.", "what"),
	}
}
