// Package simplex implements a PicoClaw channel for SimpleX Chat.
//
// SimpleX has no central server and a complex E2EE protocol, so there is no
// pure-Go SDK. The supported way to build a bot is to run the `simplex-chat`
// CLI as a local server exposing a WebSocket API (e.g. `simplex-chat -p 5225`
// -> ws://127.0.0.1:5225); PicoClaw connects to it as a client and exchanges
// JSON commands/events. This mirrors how the OneBot channel talks to an
// external bridge process.
//
// This first version supports direct (1:1) messages, inbound and outbound
// file/image attachments, and automatic acceptance of incoming contact
// requests. Group chats are intentionally out of scope.
package simplex

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
)

const defaultWSUrl = "ws://127.0.0.1:5225"

// SimplexChannel implements channels.Channel for SimpleX Chat.
type SimplexChannel struct {
	*channels.BaseChannel
	config     *config.SimpleXSettings
	autoAccept bool

	conn        *websocket.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex // guards conn
	writeMu     sync.Mutex // serializes websocket writes
	corrCounter int64

	pending   map[string]chan json.RawMessage
	pendingMu sync.Mutex

	// pendingFiles maps a downloading fileId to the context needed to publish
	// the inbound message once the download completes.
	pendingFiles   map[int64]pendingFile
	pendingFilesMu sync.Mutex
}

// pendingFile records an inbound message that is waiting for its attachment to
// finish downloading before being published to the agent.
type pendingFile struct {
	contactID int64
	chatID    string
	messageID string
	caption   string
	fileName  string
	sender    bus.SenderInfo
}

// NewSimplexChannel creates a new SimpleX channel.
func NewSimplexChannel(
	bc *config.Channel,
	cfg *config.SimpleXSettings,
	messageBus *bus.MessageBus,
) (*SimplexChannel, error) {
	if cfg.WSUrl == "" {
		cfg.WSUrl = defaultWSUrl
	}

	base := channels.NewBaseChannel("simplex", cfg, messageBus, bc.AllowFrom,
		channels.WithReasoningChannelID(bc.ReasoningChannelID),
	)

	autoAccept := true
	if cfg.AutoAccept != nil {
		autoAccept = *cfg.AutoAccept
	}

	return &SimplexChannel{
		BaseChannel:  base,
		config:       cfg,
		autoAccept:   autoAccept,
		pending:      make(map[string]chan json.RawMessage),
		pendingFiles: make(map[int64]pendingFile),
	}, nil
}

// Start connects to the simplex-chat WebSocket server and begins listening.
func (c *SimplexChannel) Start(ctx context.Context) error {
	logger.InfoCF("simplex", "Starting SimpleX channel", map[string]any{
		"ws_url": c.config.WSUrl,
	})

	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		logger.WarnCF("simplex", "Initial connection failed, will retry in background", map[string]any{
			"error": err.Error(),
		})
	} else {
		go c.listen()
		go c.initialize()
	}

	if c.config.ReconnectInterval > 0 {
		go c.reconnectLoop()
	} else if c.conn == nil {
		return fmt.Errorf("failed to connect to simplex-chat and reconnect is disabled")
	}

	c.SetRunning(true)
	logger.InfoC("simplex", "SimpleX channel started")
	return nil
}

// Stop disconnects from the server.
func (c *SimplexChannel) Stop(ctx context.Context) error {
	logger.InfoC("simplex", "Stopping SimpleX channel")
	c.SetRunning(false)

	if c.cancel != nil {
		c.cancel()
	}

	c.pendingMu.Lock()
	for corr, ch := range c.pending {
		select {
		case ch <- nil:
		default:
		}
		delete(c.pending, corr)
	}
	c.pendingMu.Unlock()

	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	return nil
}

func (c *SimplexChannel) connect() error {
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, resp, err := dialer.Dial(c.config.WSUrl, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}

	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	go c.pinger(conn)

	logger.InfoC("simplex", "WebSocket connected")
	return nil
}

func (c *SimplexChannel) pinger(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.writeMu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *SimplexChannel) reconnectLoop() {
	interval := max(time.Duration(c.config.ReconnectInterval)*time.Second, 5*time.Second)

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(interval):
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				logger.InfoC("simplex", "Attempting to reconnect...")
				if err := c.connect(); err != nil {
					logger.ErrorCF("simplex", "Reconnect failed", map[string]any{
						"error": err.Error(),
					})
				} else {
					go c.listen()
					go c.initialize()
				}
			}
		}
	}
}

// initialize runs the best-effort startup sequence: subscribe to events,
// configure the files folder, log the bot identity, and ensure a contact
// address exists so users can connect.
func (c *SimplexChannel) initialize() {
	// Ensure subscriptions are active. Harmless if the CLI already started.
	if err := c.sendCmd(cmdStartChat); err != nil {
		logger.DebugCF("simplex", "start chat failed", map[string]any{"error": err.Error()})
	}

	if c.config.FilesFolder != "" {
		if err := c.sendCmd(cmdSetFilesFolder(c.config.FilesFolder)); err != nil {
			logger.DebugCF("simplex", "set files folder failed", map[string]any{"error": err.Error()})
		}
	}

	if resp, err := c.sendCmdWait(cmdShowActiveUser, 5*time.Second); err == nil {
		var u respActiveUser
		if json.Unmarshal(resp, &u) == nil && u.User.UserId > 0 {
			logger.InfoCF("simplex", "Bot user", map[string]any{
				"user_id": u.User.UserId,
				"name":    u.User.Profile.DisplayName,
			})
		}
	}

	c.ensureAddress()
}

// ensureAddress makes sure the bot has a contact address and logs it so the
// operator can share it with users.
func (c *SimplexChannel) ensureAddress() {
	// An existing address is returned by /show_address as a "userContactLink"
	// response. The exact field nesting has changed across SimpleX versions
	// (older: contactLink.connReqContact string; newer: connLinkContact with
	// full/short links), so we scan the response for the link string instead of
	// binding to a fixed shape.
	if resp, err := c.sendCmdWait(cmdShowAddress, 5*time.Second); err == nil {
		if c.logAddress(resp) {
			return
		}
	}

	// No address yet (or none found) — create one.
	resp, err := c.sendCmdWait(cmdCreateAddress, 10*time.Second)
	if err != nil {
		logger.WarnCF("simplex", "Could not create contact address", map[string]any{"error": err.Error()})
		return
	}
	if !c.logAddress(resp) {
		logger.WarnCF("simplex",
			"Address ready but no link found in response; run /show_address in the simplex-chat CLI to view it",
			map[string]any{"response": truncate(string(resp), 500)})
	}
}

// logAddress extracts the bot's contact address from an address response and
// logs it. Returns true if a link was found.
func (c *SimplexChannel) logAddress(resp json.RawMessage) bool {
	link := findSimplexLink(resp)
	if link == "" {
		return false
	}
	logger.InfoCF("simplex", "Bot contact address (share this to let users connect)", map[string]any{
		"address": link,
	})
	return true
}

// ─── Sending ───

// sendCmd writes a command and does not wait for a reply.
func (c *SimplexChannel) sendCmd(cmd string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("simplex websocket not connected")
	}

	corr := strconv.FormatInt(atomic.AddInt64(&c.corrCounter, 1), 10)
	data, err := json.Marshal(srvRequest{CorrId: corr, Cmd: cmd})
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	return err
}

// sendCmdWait writes a command and waits for the matching reply (by corrId).
func (c *SimplexChannel) sendCmdWait(cmd string, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("simplex websocket not connected")
	}

	corr := strconv.FormatInt(atomic.AddInt64(&c.corrCounter, 1), 10)
	ch := make(chan json.RawMessage, 1)
	c.pendingMu.Lock()
	c.pending[corr] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, corr)
		c.pendingMu.Unlock()
	}()

	data, err := json.Marshal(srvRequest{CorrId: corr, Cmd: cmd})
	if err != nil {
		return nil, err
	}

	c.writeMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, fmt.Errorf("simplex command %q: channel stopped", cmd)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("simplex command %q timed out", cmd)
	case <-c.ctx.Done():
		return nil, fmt.Errorf("context canceled")
	}
}

// Send delivers a text message to a contact.
func (c *SimplexChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if strings.TrimSpace(msg.Content) == "" {
		return nil, nil
	}

	contactID, err := parseContactID(msg.ChatID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", channels.ErrSendFailed, err)
	}

	if err := c.sendCmd(cmdSendText(contactID, markdownToSimplex(msg.Content))); err != nil {
		logger.ErrorCF("simplex", "Failed to send message", map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("simplex send: %w", channels.ErrTemporary)
	}
	return nil, nil
}

// SendMedia implements channels.MediaSender, sending file/image attachments.
func (c *SimplexChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	contactID, err := parseContactID(msg.ChatID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", channels.ErrSendFailed, err)
	}

	store := c.GetMediaStore()
	if store == nil {
		return nil, fmt.Errorf("no media store available: %w", channels.ErrSendFailed)
	}

	for _, part := range msg.Parts {
		localPath, err := store.Resolve(part.Ref)
		if err != nil {
			logger.ErrorCF("simplex", "Failed to resolve media ref", map[string]any{
				"ref":   part.Ref,
				"error": err.Error(),
			})
			continue
		}
		if err := c.sendCmd(cmdSendFile(contactID, localPath, markdownToSimplex(part.Caption))); err != nil {
			logger.ErrorCF("simplex", "Failed to send media", map[string]any{"error": err.Error()})
			return nil, fmt.Errorf("simplex send media: %w", channels.ErrTemporary)
		}
	}
	return nil, nil
}

// ─── Receiving ───

func (c *SimplexChannel) listen() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			logger.DebugCF("simplex", "WebSocket read error", map[string]any{"error": err.Error()})
			c.mu.Lock()
			if c.conn == conn {
				c.conn.Close()
				c.conn = nil
			}
			c.mu.Unlock()
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var env srvResponse
		if err := json.Unmarshal(message, &env); err != nil {
			logger.DebugCF("simplex", "Failed to parse message", map[string]any{"error": err.Error()})
			continue
		}

		// Deliver command replies to any waiter; otherwise dispatch as an event.
		if env.CorrId != "" {
			c.pendingMu.Lock()
			ch, ok := c.pending[env.CorrId]
			c.pendingMu.Unlock()
			if ok {
				select {
				case ch <- env.Resp:
				default:
				}
				continue
			}
		}

		c.dispatch(env.Resp)
	}
}

func (c *SimplexChannel) dispatch(resp json.RawMessage) {
	switch t := respType(resp); t {
	case "newChatItem":
		var ev respNewChatItem
		if json.Unmarshal(resp, &ev) == nil {
			c.handleChatItem(ev.ChatItem)
		}
	case "newChatItems":
		var ev respNewChatItems
		if json.Unmarshal(resp, &ev) == nil {
			for _, item := range ev.ChatItems {
				c.handleChatItem(item)
			}
		}
	case "receivedContactRequest":
		var ev respReceivedContactRequest
		if json.Unmarshal(resp, &ev) == nil {
			c.handleContactRequest(ev.ContactRequest)
		}
	case "contactConnected":
		var ev respContactConnected
		if json.Unmarshal(resp, &ev) == nil {
			logger.InfoCF("simplex", "Contact connected", map[string]any{
				"contact_id": ev.Contact.ContactId,
				"name":       ev.Contact.displayName(),
			})
		}
	case "rcvFileComplete":
		var ev respRcvFileComplete
		if json.Unmarshal(resp, &ev) == nil {
			c.handleFileComplete(ev.ChatItem)
		}
	case "chatCmdError", "chatError", "error":
		logger.DebugCF("simplex", "Server error response", map[string]any{"payload": string(resp)})
	default:
		logger.DebugCF("simplex", "Unhandled event", map[string]any{"type": t})
	}
}

// handleContactRequest auto-accepts an incoming contact request.
func (c *SimplexChannel) handleContactRequest(req userContactRequest) {
	logger.InfoCF("simplex", "Incoming contact request", map[string]any{
		"request_id": req.ContactRequestId,
		"name":       req.Profile.DisplayName,
	})
	if !c.autoAccept {
		return
	}
	if err := c.sendCmd(cmdAcceptContact(req.ContactRequestId)); err != nil {
		logger.WarnCF("simplex", "Failed to accept contact request", map[string]any{"error": err.Error()})
	}
}

// handleChatItem processes a single inbound chat item (direct messages only).
func (c *SimplexChannel) handleChatItem(item aChatItem) {
	// Only 1:1 direct messages received from a contact.
	if item.ChatInfo.Type != "direct" || item.ChatInfo.Contact == nil {
		return
	}
	if item.ChatItem.ChatDir.Type != "directRcv" {
		return // skip our own (directSnd) and group items
	}

	ct := item.ChatInfo.Contact
	contactIDStr := strconv.FormatInt(ct.ContactId, 10)

	sender := c.senderInfo(ct)
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("simplex", "Message rejected by allowlist", map[string]any{"contact_id": ct.ContactId})
		return
	}

	text := ""
	if item.ChatItem.Content.MsgContent != nil {
		text = item.ChatItem.Content.MsgContent.Text
	}
	messageID := strconv.FormatInt(item.ChatItem.Meta.ItemId, 10)

	// If the message carries a file and inbound media is possible, start the
	// download and defer publishing until it completes.
	if file := item.ChatItem.File; file != nil && file.FileId > 0 && c.GetMediaStore() != nil {
		c.pendingFilesMu.Lock()
		c.pendingFiles[file.FileId] = pendingFile{
			contactID: ct.ContactId,
			chatID:    contactIDStr,
			messageID: messageID,
			caption:   text,
			fileName:  file.FileName,
			sender:    sender,
		}
		c.pendingFilesMu.Unlock()

		if err := c.sendCmd(cmdReceiveFile(file.FileId)); err != nil {
			logger.DebugCF("simplex", "freceive failed", map[string]any{"error": err.Error()})
		}
		return
	}

	if strings.TrimSpace(text) == "" {
		return
	}

	logger.InfoCF("simplex", "Received message", map[string]any{
		"contact_id": ct.ContactId,
		"message_id": messageID,
		"length":     len(text),
	})

	c.publish(contactIDStr, text, nil, messageID, sender)
}

// handleFileComplete publishes the inbound message once its attachment has
// finished downloading.
func (c *SimplexChannel) handleFileComplete(item aChatItem) {
	file := item.ChatItem.File
	if file == nil || file.FileId == 0 {
		return
	}

	c.pendingFilesMu.Lock()
	pf, ok := c.pendingFiles[file.FileId]
	delete(c.pendingFiles, file.FileId)
	c.pendingFilesMu.Unlock()

	localPath := file.localPath(c.config.FilesFolder)
	if localPath == "" {
		logger.DebugCF("simplex", "File complete without resolvable path", map[string]any{"file_id": file.FileId})
		return
	}

	// Fall back to the chat item's own contact if there was no pending entry
	// (e.g. the CLI auto-received the file before we registered it).
	if !ok {
		if item.ChatInfo.Type != "direct" || item.ChatInfo.Contact == nil {
			return
		}
		ct := item.ChatInfo.Contact
		sender := c.senderInfo(ct)
		if !c.IsAllowedSender(sender) {
			return
		}
		pf = pendingFile{
			contactID: ct.ContactId,
			chatID:    strconv.FormatInt(ct.ContactId, 10),
			messageID: strconv.FormatInt(item.ChatItem.Meta.ItemId, 10),
			fileName:  file.FileName,
			sender:    sender,
		}
	}

	store := c.GetMediaStore()
	if store == nil {
		return
	}

	scope := channels.BuildMediaScope("simplex", pf.chatID, pf.messageID)
	ref, err := store.Store(localPath, media.MediaMeta{
		Filename: pf.fileName,
		Source:   "simplex",
		// The file lives in the simplex-chat files folder, which PicoClaw does
		// not own, so never delete it during cleanup.
		CleanupPolicy: media.CleanupPolicyForgetOnly,
	}, scope)
	if err != nil {
		logger.WarnCF("simplex", "Failed to store inbound file", map[string]any{"error": err.Error()})
		return
	}

	content := pf.caption
	if strings.TrimSpace(content) == "" {
		content = "[file: " + pf.fileName + "]"
	}

	logger.InfoCF("simplex", "Received file", map[string]any{
		"contact_id": pf.contactID,
		"file":       pf.fileName,
	})

	c.publish(pf.chatID, content, []string{ref}, pf.messageID, pf.sender)
}

func (c *SimplexChannel) senderInfo(ct *contact) bus.SenderInfo {
	contactIDStr := strconv.FormatInt(ct.ContactId, 10)
	return bus.SenderInfo{
		Platform:    "simplex",
		PlatformID:  contactIDStr,
		CanonicalID: identity.BuildCanonicalID("simplex", contactIDStr),
		Username:    ct.LocalDisplayName,
		DisplayName: ct.displayName(),
	}
}

func (c *SimplexChannel) publish(chatID, content string, mediaRefs []string, messageID string, sender bus.SenderInfo) {
	inboundCtx := bus.InboundContext{
		Channel:   c.Name(),
		ChatID:    chatID,
		ChatType:  "direct",
		SenderID:  sender.PlatformID,
		MessageID: messageID,
		Raw: map[string]string{
			"platform": "simplex",
		},
	}
	c.HandleInboundContext(c.ctx, chatID, content, mediaRefs, inboundCtx, sender)
}

// ─── helpers ───

// findSimplexLink walks an arbitrary JSON response and returns the first value
// that looks like a SimpleX connection link. This is resilient to the field
// nesting changes between SimpleX versions (connReqContact vs connLinkContact /
// connFullLink / connShortLink). A short link is preferred when both appear.
func findSimplexLink(resp json.RawMessage) string {
	var v any
	if json.Unmarshal(resp, &v) != nil {
		return ""
	}
	var links []string
	collectSimplexLinks(v, &links)
	best := ""
	for _, l := range links {
		if best == "" || len(l) < len(best) {
			best = l // prefer the shortest (short links are nicer to share)
		}
	}
	return best
}

func collectSimplexLinks(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		if isSimplexLink(t) {
			*out = append(*out, t)
		}
	case []any:
		for _, e := range t {
			collectSimplexLinks(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectSimplexLinks(e, out)
		}
	}
}

func isSimplexLink(s string) bool {
	return strings.HasPrefix(s, "https://simplex.chat/") || strings.HasPrefix(s, "simplex:/")
}

// truncate shortens a string to at most n runes for logging.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// respType extracts the discriminator type from a resp object.
func respType(resp json.RawMessage) string {
	var h respHead
	if json.Unmarshal(resp, &h) != nil {
		return ""
	}
	return h.Type
}

// parseContactID extracts the numeric SimpleX contact id from a chat id.
func parseContactID(chatID string) (int64, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return 0, fmt.Errorf("empty chat id")
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid simplex contact id %q", chatID)
	}
	return id, nil
}
