package simplex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

func newTestChannel(t *testing.T) (*SimplexChannel, *bus.MessageBus) {
	t.Helper()
	msgBus := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelSimplex, Enabled: true, AllowFrom: []string{"*"}}
	cfg := &config.SimpleXSettings{}
	ch, err := NewSimplexChannel(bc, cfg, msgBus)
	if err != nil {
		t.Fatalf("NewSimplexChannel() error = %v", err)
	}
	ch.ctx = context.Background()
	return ch, msgBus
}

func TestNewSimplexChannelDefaults(t *testing.T) {
	ch, _ := newTestChannel(t)
	if ch.config.WSUrl != defaultWSUrl {
		t.Errorf("WSUrl = %q, want default %q", ch.config.WSUrl, defaultWSUrl)
	}
	if !ch.autoAccept {
		t.Error("autoAccept should default to true")
	}
	if ch.Name() != "simplex" {
		t.Errorf("Name() = %q, want simplex", ch.Name())
	}
}

func TestAutoAcceptDisabled(t *testing.T) {
	msgBus := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelSimplex, Enabled: true}
	no := false
	ch, err := NewSimplexChannel(bc, &config.SimpleXSettings{AutoAccept: &no}, msgBus)
	if err != nil {
		t.Fatalf("NewSimplexChannel() error = %v", err)
	}
	if ch.autoAccept {
		t.Error("autoAccept should be false when configured off")
	}
}

func TestCmdSendText(t *testing.T) {
	got := cmdSendText(42, "hello world")
	want := `/_send @42 json [{"msgContent":{"type":"text","text":"hello world"}}]`
	if got != want {
		t.Errorf("cmdSendText() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestCmdSendTextEscaping(t *testing.T) {
	// Text with quotes/newlines must be JSON-escaped so the command stays valid.
	got := cmdSendText(7, "she said \"hi\"\nbye")
	var prefix string
	var payload []composedMessage
	if err := scanSendCmd(got, &prefix, &payload); err != nil {
		t.Fatalf("command not parseable: %v (cmd=%q)", err, got)
	}
	if prefix != "/_send @7 json " {
		t.Errorf("prefix = %q", prefix)
	}
	if len(payload) != 1 || payload[0].MsgContent.Text != "she said \"hi\"\nbye" {
		t.Errorf("round-tripped payload mismatch: %+v", payload)
	}
}

func TestCmdSendFile(t *testing.T) {
	got := cmdSendFile(3, "/tmp/pic.png", "caption")
	want := `/_send @3 json [{"filePath":"/tmp/pic.png","msgContent":{"type":"text","text":"caption"}}]`
	if got != want {
		t.Errorf("cmdSendFile() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestCmdHelpers(t *testing.T) {
	if got := cmdAcceptContact(11); got != "/_accept 11" {
		t.Errorf("cmdAcceptContact() = %q", got)
	}
	if got := cmdReceiveFile(99); got != "/freceive 99" {
		t.Errorf("cmdReceiveFile() = %q", got)
	}
	if got := cmdSetFilesFolder("/var/files"); got != "/_files_folder /var/files" {
		t.Errorf("cmdSetFilesFolder() = %q", got)
	}
}

func TestParseContactID(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"42", 42, false},
		{" 42 ", 42, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		got, err := parseContactID(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseContactID(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("parseContactID(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestRespType(t *testing.T) {
	if got := respType(json.RawMessage(`{"type":"newChatItems","x":1}`)); got != "newChatItems" {
		t.Errorf("respType() = %q", got)
	}
	if got := respType(json.RawMessage(`not json`)); got != "" {
		t.Errorf("respType(invalid) = %q, want empty", got)
	}
}

func TestCiFileLocalPath(t *testing.T) {
	tests := []struct {
		name        string
		file        *ciFile
		filesFolder string
		want        string
	}{
		{"nil file", nil, "/f", ""},
		{"fileSource absolute", &ciFile{FileSource: &ciFileSource{FilePath: "/abs/a.png"}}, "/f", "/abs/a.png"},
		{"fileSource relative joined", &ciFile{FileSource: &ciFileSource{FilePath: "a.png"}}, "/f", "/f/a.png"},
		{"legacy filePath fallback", &ciFile{FilePath: "b.png"}, "/f", "/f/b.png"},
		{
			"fileSource preferred over legacy",
			&ciFile{FilePath: "old.png", FileSource: &ciFileSource{FilePath: "/new.png"}},
			"/f", "/new.png",
		},
		{"relative no folder", &ciFile{FilePath: "c.png"}, "", "c.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.file.localPath(tt.filesFolder); got != tt.want {
				t.Errorf("localPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContactDisplayName(t *testing.T) {
	c := contact{LocalDisplayName: "alice_1", Profile: profile{DisplayName: "Alice"}}
	if got := c.displayName(); got != "Alice" {
		t.Errorf("displayName() = %q, want Alice", got)
	}
	c2 := contact{LocalDisplayName: "bob_2"}
	if got := c2.displayName(); got != "bob_2" {
		t.Errorf("displayName() fallback = %q, want bob_2", got)
	}
}

func TestFindSimplexLink(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "legacy connReqContact nested",
			json: `{"type":"userContactLink","contactLink":{"connReqContact":"https://simplex.chat/contact#abc","autoAccept":null}}`,
			want: "https://simplex.chat/contact#abc",
		},
		{
			name: "legacy connReqContact flat",
			json: `{"type":"userContactLinkCreated","connReqContact":"https://simplex.chat/contact#flat"}`,
			want: "https://simplex.chat/contact#flat",
		},
		{
			// Newer shape: full + short links; the shorter one is preferred.
			name: "newer connLinkContact full+short",
			json: `{"type":"userContactLink","contactLink":{"connLinkContact":{"connFullLink":"https://simplex.chat/contact#/?v=2-7&long=yyyyyyyyyyyyyyyyyyyy","connShortLink":"https://simplex.chat/a#short"}}}`,
			want: "https://simplex.chat/a#short",
		},
		{
			name: "uri scheme form",
			json: `{"contactLink":{"connReqContact":"simplex:/contact#/?v=2"}}`,
			want: "simplex:/contact#/?v=2",
		},
		{
			name: "no link present",
			json: `{"type":"chatCmdError","chatError":{"type":"error"}}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findSimplexLink(json.RawMessage(tt.json)); got != tt.want {
				t.Errorf("findSimplexLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDispatchDirectMessage feeds a newChatItems event and asserts an inbound
// text message is published.
func TestDispatchDirectMessage(t *testing.T) {
	ch, msgBus := newTestChannel(t)

	payload := `{
		"type": "newChatItems",
		"chatItems": [{
			"chatInfo": {"type": "direct", "contact": {"contactId": 5, "localDisplayName": "alice", "profile": {"displayName": "Alice"}}},
			"chatItem": {
				"chatDir": {"type": "directRcv"},
				"meta": {"itemId": 100, "itemText": "hi bot"},
				"content": {"type": "rcvMsgContent", "msgContent": {"type": "text", "text": "hi bot"}}
			}
		}]
	}`
	ch.dispatch(json.RawMessage(payload))

	msg := mustReceiveInbound(t, msgBus)
	if msg.Content != "hi bot" {
		t.Errorf("Content = %q, want %q", msg.Content, "hi bot")
	}
	if msg.ChatID != "5" {
		t.Errorf("ChatID = %q, want 5", msg.ChatID)
	}
	if msg.Context.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", msg.Context.ChatType)
	}
	if msg.Sender.DisplayName != "Alice" {
		t.Errorf("Sender.DisplayName = %q, want Alice", msg.Sender.DisplayName)
	}
	if msg.MessageID != "100" {
		t.Errorf("MessageID = %q, want 100", msg.MessageID)
	}
}

// TestDispatchIgnoresOwnMessages verifies directSnd (sent by the bot) items are
// not republished.
func TestDispatchIgnoresOwnMessages(t *testing.T) {
	ch, msgBus := newTestChannel(t)
	payload := `{
		"type": "newChatItem",
		"chatItem": {
			"chatInfo": {"type": "direct", "contact": {"contactId": 5, "profile": {"displayName": "Alice"}}},
			"chatItem": {
				"chatDir": {"type": "directSnd"},
				"meta": {"itemId": 101, "itemText": "my own reply"},
				"content": {"type": "sndMsgContent", "msgContent": {"type": "text", "text": "my own reply"}}
			}
		}
	}`
	ch.dispatch(json.RawMessage(payload))
	assertNoInbound(t, msgBus)
}

// TestDispatchAllowlist verifies that a disallowed sender is dropped.
func TestDispatchAllowlist(t *testing.T) {
	msgBus := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelSimplex, Enabled: true, AllowFrom: []string{"simplex:999"}}
	ch, err := NewSimplexChannel(bc, &config.SimpleXSettings{}, msgBus)
	if err != nil {
		t.Fatalf("NewSimplexChannel() error = %v", err)
	}
	ch.ctx = context.Background()

	payload := `{
		"type": "newChatItem",
		"chatItem": {
			"chatInfo": {"type": "direct", "contact": {"contactId": 5, "profile": {"displayName": "Alice"}}},
			"chatItem": {"chatDir": {"type": "directRcv"}, "meta": {"itemId": 102}, "content": {"type": "rcvMsgContent", "msgContent": {"type": "text", "text": "hi"}}}
		}
	}`
	ch.dispatch(json.RawMessage(payload))
	assertNoInbound(t, msgBus)
}

// TestInboundFileFlow verifies a message with a file is published only after
// rcvFileComplete, carrying a media ref.
func TestInboundFileFlow(t *testing.T) {
	ch, msgBus := newTestChannel(t)
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(filePath, []byte("img"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// 1) Incoming message carrying a file: nothing published yet.
	newItems := `{
		"type": "newChatItems",
		"chatItems": [{
			"chatInfo": {"type": "direct", "contact": {"contactId": 8, "profile": {"displayName": "Bob"}}},
			"chatItem": {
				"chatDir": {"type": "directRcv"},
				"meta": {"itemId": 200, "itemText": "look"},
				"content": {"type": "rcvMsgContent", "msgContent": {"type": "image", "text": "look"}},
				"file": {"fileId": 77, "fileName": "photo.jpg"}
			}
		}]
	}`
	ch.dispatch(json.RawMessage(newItems))
	assertNoInbound(t, msgBus)

	// 2) Download completes with an absolute path.
	complete := `{
		"type": "rcvFileComplete",
		"chatItem": {
			"chatInfo": {"type": "direct", "contact": {"contactId": 8, "profile": {"displayName": "Bob"}}},
			"chatItem": {
				"chatDir": {"type": "directRcv"},
				"meta": {"itemId": 200},
				"content": {"type": "rcvMsgContent", "msgContent": {"type": "image", "text": "look"}},
				"file": {"fileId": 77, "fileName": "photo.jpg", "fileSource": {"filePath": "` + filePath + `"}}
			}
		}
	}`
	ch.dispatch(json.RawMessage(complete))

	msg := mustReceiveInbound(t, msgBus)
	if msg.Content != "look" {
		t.Errorf("Content = %q, want look (caption)", msg.Content)
	}
	if len(msg.Media) != 1 {
		t.Fatalf("Media count = %d, want 1", len(msg.Media))
	}
	resolved, err := store.Resolve(msg.Media[0])
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved != filePath {
		t.Errorf("resolved media path = %q, want %q", resolved, filePath)
	}
}

func TestHandleContactRequestNoPanicWhenDisconnected(t *testing.T) {
	ch, _ := newTestChannel(t)
	// No connection; should log a warning but not panic.
	ch.handleContactRequest(userContactRequest{ContactRequestId: 1, Profile: profile{DisplayName: "x"}})
}

func TestMarkdownToSimplex(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain unchanged", "just text", "just text"},
		{"bold", "a **b** c", "a *b* c"},
		{"italic", "a *b* c", "a _b_ c"},
		{"bold and italic", "**b** and _i_", "*b* and _i_"},
		{"strikethrough", "~~gone~~", "~gone~"},
		{"inline code", "use `go test`", "use `go test`"},
		{"heading", "# Title\n\nbody", "*Title*\n\nbody"},
		{"link", "see [docs](https://x.io)", "see docs (https://x.io)"},
		{"bare-ish link same label", "[https://x.io](https://x.io)", "https://x.io"},
		{"bullet list", "- one\n- two", "• one\n• two"},
		{"ordered list", "1. one\n2. two", "1. one\n2. two"},
		{
			name: "table",
			in:   "| Name | Count |\n| --- | --- |\n| alice | 12 |\n| bob | 3 |",
			want: "Name | Count\n--- | ---\nalice | 12\nbob | 3",
		},
		{"fenced code", "```\nx := 1\n```", "`x := 1`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markdownToSimplex(tt.in); got != tt.want {
				t.Errorf("markdownToSimplex(%q) =\n  %q\nwant\n  %q", tt.in, got, tt.want)
			}
		})
	}
}

// ─── helpers ───

func mustReceiveInbound(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case msg := <-mb.InboundChan():
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
		return bus.InboundMessage{}
	}
}

func assertNoInbound(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-mb.InboundChan():
		t.Fatalf("unexpected inbound message: %+v", msg)
	case <-time.After(150 * time.Millisecond):
	}
}

// scanSendCmd splits a "/_send @<id> json <payload>" command, decoding the JSON
// payload into dst and reporting the leading prefix (through " json ").
func scanSendCmd(cmd string, prefix *string, dst *[]composedMessage) error {
	head, payload, ok := strings.Cut(cmd, " json ")
	if !ok {
		return fmt.Errorf("no json marker in %q", cmd)
	}
	*prefix = head + " json "
	return json.Unmarshal([]byte(payload), dst)
}
