package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"agent-room/internal/models"

	"github.com/gorilla/websocket"
)

type directAIOptions struct {
	roomInput      string
	relayInput     string
	agentID        string
	agentLabel     string
	say            string
	targetID       string
	hello          bool
	requestReply   bool
	turnBudget     int
	tailLimit      int
	wait           time.Duration
	includeRawJSON bool
}

type directAIEndpoints struct {
	RoomID          string
	HTTPBase        string
	MessagesURL     string
	ParticipantsURL string
	WebSocketURL    string
}

func runDirectAI(args []string) error {
	opts := directAIOptions{}
	fs := flag.NewFlagSet("direct-ai", flag.ExitOnError)
	fs.StringVar(&opts.roomInput, "room", firstNonEmpty(os.Getenv("AGENT_ROOM_ROOM_ID"), os.Getenv("AGENT_ROOM_ROOM_URL")), "room id or full room URL")
	fs.StringVar(&opts.relayInput, "relay", firstNonEmpty(os.Getenv("AGENT_ROOM_HTTP_URL"), os.Getenv("AGENT_ROOM_RELAY_URL")), "relay origin/base URL; optional when -room is a full URL")
	fs.StringVar(&opts.agentID, "agent-id", firstNonEmpty(os.Getenv("AGENT_ROOM_AGENT_ID"), defaultDirectAIAgentID()), "direct AI agent id")
	fs.StringVar(&opts.agentLabel, "agent-label", "", "display label for the direct AI agent")
	fs.StringVar(&opts.say, "say", "", "send one chat message as this direct AI agent")
	fs.StringVar(&opts.targetID, "target", "", "target agent/user id for -say")
	fs.BoolVar(&opts.hello, "hello", false, "send a standard direct-AI introduction message")
	fs.BoolVar(&opts.requestReply, "request-reply", false, "ask the target bridge agent to reply")
	fs.IntVar(&opts.turnBudget, "turn-budget", 0, "reply turn budget when requesting a bridge reply")
	fs.IntVar(&opts.tailLimit, "tail", 0, "print recent room messages after any sends; default 20 when no action is specified")
	fs.DurationVar(&opts.wait, "wait", 0, "open a room websocket and wait until this agent is addressed, e.g. 2m")
	fs.BoolVar(&opts.includeRawJSON, "json", false, "print raw message JSON for -tail and -wait")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts.agentID = sanitizeIDPart(opts.agentID)
	if opts.agentID == "" {
		return fmt.Errorf("agent id is required")
	}
	if strings.TrimSpace(opts.agentLabel) == "" {
		opts.agentLabel = opts.agentID
	}
	if opts.requestReply && opts.turnBudget <= 0 {
		opts.turnBudget = 1
	}

	endpoints, err := directAIEndpointsFrom(opts.roomInput, opts.relayInput, opts.agentID, opts.agentLabel)
	if err != nil {
		return err
	}

	didAction := opts.hello || strings.TrimSpace(opts.say) != "" || opts.tailLimit > 0 || opts.wait > 0
	if !didAction {
		opts.tailLimit = 20
	}

	fmt.Fprintf(os.Stderr, "→ room  %s\n→ agent @%s\n→ relay %s\n", endpoints.RoomID, opts.agentID, endpoints.HTTPBase)

	ctx := context.Background()
	if opts.hello {
		content := fmt.Sprintf("我是 @%s，已用 Direct AI CLI 接入。需要我排查或处理时直接 @%s。", opts.agentID, opts.agentID)
		msg, err := postDirectAIMessage(ctx, endpoints.MessagesURL, opts, content)
		if err != nil {
			return err
		}
		fmt.Printf("sent hello: %s\n", msg.ID)
	}
	if strings.TrimSpace(opts.say) != "" {
		msg, err := postDirectAIMessage(ctx, endpoints.MessagesURL, opts, opts.say)
		if err != nil {
			return err
		}
		fmt.Printf("sent message: %s\n", msg.ID)
	}
	if opts.tailLimit > 0 {
		if err := printDirectAITail(ctx, endpoints, opts); err != nil {
			return err
		}
	}
	if opts.wait > 0 {
		if err := waitDirectAI(ctx, endpoints, opts); err != nil {
			return err
		}
	}
	return nil
}

func directAIEndpointsFrom(roomInput, relayInput, agentID, agentLabel string) (directAIEndpoints, error) {
	roomID := parseRoomIDFromInput(roomInput)
	if roomID == "" {
		return directAIEndpoints{}, fmt.Errorf("room id is required (pass -room with a room id or URL)")
	}
	httpBase := directAIHTTPBase(roomInput, relayInput)
	if httpBase == "" {
		httpBase = "http://127.0.0.1:8080"
	}
	httpBase = strings.TrimRight(httpBase, "/")
	wsBase := directAIWSBase(httpBase)
	encodedRoom := url.PathEscape(roomID)
	q := url.Values{}
	q.Set("client_id", agentID)
	q.Set("client_kind", string(models.SenderKindAgent))
	q.Set("client_label", agentLabel)
	q.Set("principal_id", agentID)
	q.Set("principal_label", agentLabel)
	return directAIEndpoints{
		RoomID:          roomID,
		HTTPBase:        httpBase,
		MessagesURL:     httpBase + "/v1/rooms/" + encodedRoom + "/messages",
		ParticipantsURL: httpBase + "/v1/rooms/" + encodedRoom + "/participants",
		WebSocketURL:    wsBase + "/v1/rooms/" + encodedRoom + "/ws?" + q.Encode(),
	}, nil
}

func directAIHTTPBase(roomInput, relayInput string) string {
	if base := originFromURL(roomInput); base != "" {
		return base
	}
	if base := originFromURL(relayInput); base != "" {
		return base
	}
	return ""
}

func originFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return ""
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func directAIWSBase(httpBase string) string {
	u, err := url.Parse(httpBase)
	if err != nil || u.Host == "" {
		return strings.TrimRight(normalizeWSScheme(httpBase), "/")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func defaultDirectAIAgentID() string {
	base := firstNonEmpty(os.Getenv("USER"), os.Getenv("USERNAME"), "local")
	base = sanitizeIDPart(base)
	if base == "" {
		base = "local"
	}
	return "ai-" + base
}

func postDirectAIMessage(ctx context.Context, endpoint string, opts directAIOptions, content string) (models.ChatMessage, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return models.ChatMessage{}, fmt.Errorf("message content is empty")
	}
	msg := models.ChatMessage{
		Type:           models.MessageTypeChat,
		SenderID:       opts.agentID,
		SenderKind:     models.SenderKindAgent,
		TargetID:       strings.TrimSpace(opts.targetID),
		Content:        content,
		ReplyRequested: opts.requestReply,
		TurnBudget:     opts.turnBudget,
		Metadata: map[string]string{
			"source": "direct-ai-cli",
			"role":   "conversational-agent",
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return models.ChatMessage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return models.ChatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return models.ChatMessage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return models.ChatMessage{}, fmt.Errorf("post message: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out models.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return models.ChatMessage{}, fmt.Errorf("decode posted message: %w", err)
	}
	return out, nil
}

func printDirectAITail(ctx context.Context, endpoints directAIEndpoints, opts directAIOptions) error {
	participants, err := fetchDirectAIParticipants(ctx, endpoints.ParticipantsURL)
	if err == nil {
		fmt.Println("participants:")
		if len(participants) == 0 {
			fmt.Println("- none online")
		}
		for _, p := range participants {
			fmt.Printf("- @%s kind=%s label=%q connections=%d\n", p.ID, p.Kind, p.Label, p.ConnectionCount)
		}
	} else {
		fmt.Fprintf(os.Stderr, "warn: participants unavailable: %v\n", err)
	}

	messages, err := fetchDirectAIMessages(ctx, endpoints.MessagesURL, opts.tailLimit)
	if err != nil {
		return err
	}
	fmt.Printf("recent messages (%d):\n", len(messages))
	for _, msg := range messages {
		if opts.includeRawJSON {
			encoded, _ := json.Marshal(msg)
			fmt.Println(string(encoded))
			continue
		}
		fmt.Println(formatDirectAIMessage(msg, opts.agentID))
	}
	return nil
}

func fetchDirectAIParticipants(ctx context.Context, endpoint string) ([]models.Participant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out []models.Participant
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fetchDirectAIMessages(ctx context.Context, endpoint string, limit int) ([]models.ChatMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?limit="+url.QueryEscape(fmt.Sprint(limit)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get messages: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out []models.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return out, nil
}

func waitDirectAI(ctx context.Context, endpoints directAIEndpoints, opts directAIOptions) error {
	waitCtx, cancel := context.WithTimeout(ctx, opts.wait)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(waitCtx, endpoints.WebSocketURL, nil)
	if err != nil {
		return fmt.Errorf("direct-ai websocket: %w", err)
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "→ online for up to %s; waiting for @%s or target_id=%s\n", opts.wait, opts.agentID, opts.agentID)
	if deadline, ok := waitCtx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}

	for {
		var msg models.ChatMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				fmt.Println("wait timeout: no addressed message")
				return nil
			}
			if waitCtx.Err() != nil {
				fmt.Println("wait timeout: no addressed message")
				return nil
			}
			return fmt.Errorf("direct-ai websocket read: %w", err)
		}
		if opts.includeRawJSON {
			encoded, _ := json.Marshal(msg)
			fmt.Println(string(encoded))
		} else {
			fmt.Println(formatDirectAIMessage(msg, opts.agentID))
		}
		if directAIAddressed(msg, opts.agentID) {
			return nil
		}
	}
}

func formatDirectAIMessage(msg models.ChatMessage, agentID string) string {
	marker := ""
	if directAIAddressed(msg, agentID) {
		marker = " [ADDRESSED]"
	}
	target := ""
	if strings.TrimSpace(msg.TargetID) != "" {
		target = " -> @" + msg.TargetID
	}
	when := "unknown-time"
	if !msg.CreatedAt.IsZero() {
		when = msg.CreatedAt.Local().Format("2006-01-02 15:04:05")
	}
	content := strings.ReplaceAll(strings.TrimSpace(msg.Content), "\n", " ")
	if len(content) > 600 {
		content = content[:600] + "..."
	}
	return fmt.Sprintf("%s %s @%s%s: %s%s", when, msg.Type, msg.SenderID, target, content, marker)
}

func directAIAddressed(msg models.ChatMessage, agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || msg.SenderID == agentID {
		return false
	}
	if msg.Type != "" && msg.Type != models.MessageTypeChat {
		return false
	}
	if msg.TargetID == agentID {
		return true
	}
	pattern := regexp.MustCompile(`(^|\s)@([a-zA-Z0-9_-]{1,64})(\s|$|[,:;.!?，。！？、])`)
	for _, match := range pattern.FindAllStringSubmatch(msg.Content, -1) {
		if len(match) >= 3 && match[2] == agentID {
			return true
		}
	}
	return false
}
