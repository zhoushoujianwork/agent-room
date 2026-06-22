package agent

import (
	"context"
	"regexp"
	"strings"
	"time"

	"agent-room/internal/models"
	"agent-room/pkg/id"
)

type Responder struct {
	agentID      string
	agentLabel   string
	capabilities string
	systemPrompt string
	contextDoc   string
	historyLimit int
	maxTurns     int
	relayHTTPURL string
	provider     models.AgentProvider
}

func NewResponder(agentID, agentLabel, capabilities, systemPrompt, contextDoc string, historyLimit int, maxTurns int, relayHTTPURL string, provider models.AgentProvider) *Responder {
	if historyLimit <= 0 {
		historyLimit = 30
	}
	if agentLabel == "" {
		agentLabel = agentID
	}
	if maxTurns <= 0 {
		maxTurns = 1
	}
	return &Responder{
		agentID:      agentID,
		agentLabel:   agentLabel,
		capabilities: capabilities,
		systemPrompt: systemPrompt,
		contextDoc:   contextDoc,
		historyLimit: historyLimit,
		maxTurns:     maxTurns,
		relayHTTPURL: relayHTTPURL,
		provider:     provider,
	}
}

func (r *Responder) ShouldReply(msg models.ChatMessage) bool {
	if msg.Type != "" && msg.Type != models.MessageTypeChat {
		return false
	}
	if strings.TrimSpace(msg.Content) == "" {
		return false
	}
	if msg.SenderID == r.agentID {
		return false
	}
	if !msg.IsAddressedTo(r.agentID) {
		return false
	}
	return msg.ReplyRequested && msg.TurnBudget > 0
}

func (r *Responder) Reply(ctx context.Context, msg models.ChatMessage, history []models.ChatMessage, roomSummary string, onEvent func(models.ProviderEvent), requestPermission func(context.Context, models.PermissionRequest) (models.PermissionDecision, error), delegateExec func(context.Context, models.DelegateExecRequest) (models.DelegateExecResult, error)) (models.ChatMessage, error) {
	resp, err := r.provider.Complete(ctx, models.ProviderRequest{
		RoomID:            msg.RoomID,
		AgentID:           r.agentID,
		AgentLabel:        r.agentLabel,
		Capabilities:      r.capabilities,
		Input:             msg,
		History:           replyAwareHistory(history, r.historyLimit, msg),
		Attachments:       resolveAttachments(msg, r.relayHTTPURL),
		RoomSummary:       roomSummary,
		ContextDoc:        r.contextDoc,
		SystemPrompt:      r.systemPrompt,
		MaxTurns:          r.maxTurns,
		RelayHTTPURL:      r.relayHTTPURL,
		OnEvent:           onEvent,
		RequestPermission: requestPermission,
		DelegateExec:      delegateExec,
	})
	if err != nil {
		return models.ChatMessage{}, err
	}

	turnBudget := msg.TurnBudget - 1
	content, suppressReply := stripNoReplyMarker(resp.Content)
	targetID := r.replyTarget(msg.SenderID, content, history, turnBudget)
	replyRequested := turnBudget > 0 && !suppressReply
	if msg.SenderKind == models.SenderKindAgent && targetID == msg.SenderID && !explicitlyMentionsAgent(content, msg.SenderID) {
		replyRequested = false
	}
	return models.ChatMessage{
		ID:             id.New("msg"),
		RoomID:         msg.RoomID,
		Type:           models.MessageTypeChat,
		SenderID:       r.agentID,
		SenderKind:     models.SenderKindAgent,
		TargetID:       targetID,
		Content:        content,
		ReplyRequested: replyRequested,
		TurnBudget:     turnBudget,
		CreatedAt:      time.Now().UTC(),
		Metadata: map[string]string{
			"provider":   r.provider.Name(),
			"session_id": resp.SessionID,
		},
	}, nil
}

var replyMentionPattern = regexp.MustCompile(`(?:^|\s)@([a-zA-Z0-9_-]{1,64})(?:\s|$|[,:;.!?，。！？、])`)
var noReplyMarkerPattern = regexp.MustCompile(`(?i)\s*\[\[\s*no[-_\s]?reply\s*\]\]\s*`)

func stripNoReplyMarker(content string) (string, bool) {
	suppress := noReplyMarkerPattern.FindStringIndex(content) != nil
	cleaned := noReplyMarkerPattern.ReplaceAllString(content, "")
	return strings.TrimSpace(cleaned), suppress
}

func explicitlyMentionsAgent(content, agentID string) bool {
	content = strings.TrimSpace(content)
	agentID = strings.TrimSpace(agentID)
	if content == "" || agentID == "" {
		return false
	}
	return strings.Contains(content, "@"+agentID)
}

func (r *Responder) replyTarget(fallback, content string, history []models.ChatMessage, turnBudget int) string {
	if turnBudget <= 0 {
		return fallback
	}
	peers := knownAgentPeers(history, r.agentID)
	if len(peers) == 0 {
		return fallback
	}
	for _, match := range replyMentionPattern.FindAllStringSubmatch(content, -1) {
		if len(match) < 2 {
			continue
		}
		target := strings.TrimSpace(match[1])
		if peers[target] {
			return target
		}
	}
	return fallback
}

func knownAgentPeers(history []models.ChatMessage, selfID string) map[string]bool {
	peers := make(map[string]bool)
	for _, msg := range history {
		if msg.Type != models.MessageTypePresence || msg.SenderKind != models.SenderKindAgent {
			continue
		}
		id := strings.TrimSpace(msg.SenderID)
		if id == "" || id == selfID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(msg.Metadata["mode"]), "executor") {
			continue
		}
		peers[id] = true
	}
	return peers
}

// resolveAttachments 把入站消息 metadata 里的附件引用解析成可直接 GET 的
// 绝对 URL(指向 relay 的附件下载端点)。relay HTTP base 未知时返回 nil ——
// agent 反正拉不到相对路径。附件 URL 含 room id,授权边界与房间消息一致。
func resolveAttachments(msg models.ChatMessage, relayHTTPURL string) []models.ProviderAttachment {
	base := strings.TrimRight(strings.TrimSpace(relayHTTPURL), "/")
	if base == "" {
		return nil
	}
	refs := models.DecodeAttachmentList(msg.Metadata[models.MetadataKeyAttachments])
	if len(refs) == 0 {
		return nil
	}
	out := make([]models.ProviderAttachment, 0, len(refs))
	for _, ref := range refs {
		out = append(out, models.ProviderAttachment{
			URL:  base + "/v1/rooms/" + msg.RoomID + "/attachments/" + ref.ID,
			MIME: ref.MIME,
			Name: ref.Name,
		})
	}
	return out
}

func replyAwareHistory(messages []models.ChatMessage, limit int, input models.ChatMessage) []models.ChatMessage {
	tail := last(messages, limit)
	replyTo := ""
	if input.Metadata != nil {
		replyTo = strings.TrimSpace(input.Metadata["reply_to"])
	}
	if replyTo == "" || containsReplyReference(tail, replyTo) {
		return tail
	}

	targetKey := ""
	for _, m := range messages {
		if matchesReplyReference(m, replyTo) {
			targetKey = historyKey(m)
			break
		}
	}
	if targetKey == "" {
		return tail
	}

	keep := make(map[string]bool, len(tail)+1)
	for _, m := range tail {
		if key := historyKey(m); key != "" {
			keep[key] = true
		}
	}
	keep[targetKey] = true

	out := make([]models.ChatMessage, 0, len(tail)+1)
	for _, m := range messages {
		if key := historyKey(m); key != "" && keep[key] {
			out = append(out, m)
		}
	}
	return out
}

func containsReplyReference(messages []models.ChatMessage, ref string) bool {
	for _, m := range messages {
		if matchesReplyReference(m, ref) {
			return true
		}
	}
	return false
}

func matchesReplyReference(msg models.ChatMessage, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if msg.ID == ref {
		return true
	}
	if !msg.CreatedAt.IsZero() {
		return msg.CreatedAt.Format(time.RFC3339Nano) == ref || msg.CreatedAt.Format(time.RFC3339) == ref
	}
	return false
}

func historyKey(msg models.ChatMessage) string {
	if strings.TrimSpace(msg.ID) != "" {
		return "id:" + strings.TrimSpace(msg.ID)
	}
	if !msg.CreatedAt.IsZero() {
		return "at:" + msg.CreatedAt.Format(time.RFC3339Nano)
	}
	return ""
}

func last(messages []models.ChatMessage, limit int) []models.ChatMessage {
	if limit <= 0 {
		return messages
	}
	// Presence messages drive participant + executor-peer awareness in the
	// prompt and are small and bounded, so they are always retained. The
	// limit applies only to the conversational tail (chat/command/etc), so a
	// long backfilled history can't push an executor's early "joined" notice
	// out of the window and silently disable delegation.
	convCount := 0
	for _, m := range messages {
		if m.Type != models.MessageTypePresence {
			convCount++
		}
	}
	if convCount <= limit {
		return messages
	}
	drop := convCount - limit // number of oldest conversational messages to skip
	out := make([]models.ChatMessage, 0, len(messages))
	for _, m := range messages {
		if m.Type != models.MessageTypePresence && drop > 0 {
			drop--
			continue
		}
		out = append(out, m)
	}
	return out
}
