package simplex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// This file describes the subset of the SimpleX Chat WebSocket protocol that
// the channel needs. The wire protocol is the one exposed by the
// `simplex-chat` CLI server (e.g. `simplex-chat -p 5225` -> ws://127.0.0.1:5225)
// and mirrors the official TypeScript client
// (simplex-chat/packages/simplex-chat-client/typescript).
//
// Requests are `{"corrId": "<id>", "cmd": "<command string>"}`.
// Replies are `{"corrId": "<id>", "resp": {"type": "...", ...}}` and
// unsolicited events are `{"resp": {"type": "...", ...}}` (no corrId).
// The `resp.type` string is the discriminator in both cases.

// srvRequest is the outbound command envelope.
type srvRequest struct {
	CorrId string `json:"corrId"`
	Cmd    string `json:"cmd"`
}

// srvResponse is the inbound envelope for both command replies and events.
type srvResponse struct {
	CorrId string          `json:"corrId,omitempty"`
	Resp   json.RawMessage `json:"resp"`
}

// respHead extracts just the discriminator from a resp object.
type respHead struct {
	Type string `json:"type"`
}

// ─── Data structures (only the fields the channel consumes) ───

type profile struct {
	DisplayName string `json:"displayName"`
	FullName    string `json:"fullName"`
}

type contact struct {
	ContactId        int64   `json:"contactId"`
	LocalDisplayName string  `json:"localDisplayName"`
	Profile          profile `json:"profile"`
}

// displayName returns the best human-readable name for the contact.
func (c contact) displayName() string {
	if c.Profile.DisplayName != "" {
		return c.Profile.DisplayName
	}
	return c.LocalDisplayName
}

type userContactRequest struct {
	ContactRequestId int64   `json:"contactRequestId"`
	LocalDisplayName string  `json:"localDisplayName"`
	Profile          profile `json:"profile"`
}

type chatInfo struct {
	Type    string   `json:"type"` // "direct" | "group" | "contactRequest"
	Contact *contact `json:"contact,omitempty"`
}

type msgContent struct {
	Type string `json:"type"` // "text" | "image" | "file" | "voice" | "video" | "link"
	Text string `json:"text"`
}

type ciContent struct {
	Type       string      `json:"type"` // "rcvMsgContent" | "sndMsgContent" | ...
	MsgContent *msgContent `json:"msgContent,omitempty"`
}

type ciFileSource struct {
	FilePath string `json:"filePath"`
}

type ciFile struct {
	FileId     int64         `json:"fileId"`
	FileName   string        `json:"fileName"`
	FileSource *ciFileSource `json:"fileSource,omitempty"`
	// FilePath is a fallback used by older CLI versions that put the path
	// directly on the file object instead of under fileSource.
	FilePath string `json:"filePath,omitempty"`
}

// localPath resolves the on-disk path of a received file, joining a relative
// path against filesFolder when one is configured.
func (f *ciFile) localPath(filesFolder string) string {
	if f == nil {
		return ""
	}
	p := f.FilePath
	if f.FileSource != nil && f.FileSource.FilePath != "" {
		p = f.FileSource.FilePath
	}
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) && filesFolder != "" {
		p = filepath.Join(filesFolder, p)
	}
	return p
}

type chatDir struct {
	Type string `json:"type"` // "directRcv" | "directSnd" | "groupRcv" | "groupSnd"
}

type ciMeta struct {
	ItemId   int64  `json:"itemId"`
	ItemText string `json:"itemText"`
}

type chatItem struct {
	ChatDir chatDir   `json:"chatDir"`
	Meta    ciMeta    `json:"meta"`
	Content ciContent `json:"content"`
	File    *ciFile   `json:"file,omitempty"`
}

type aChatItem struct {
	ChatInfo chatInfo `json:"chatInfo"`
	ChatItem chatItem `json:"chatItem"`
}

// ─── Event payloads ───

type respNewChatItem struct {
	ChatItem aChatItem `json:"chatItem"`
}

type respNewChatItems struct {
	ChatItems []aChatItem `json:"chatItems"`
}

type respReceivedContactRequest struct {
	ContactRequest userContactRequest `json:"contactRequest"`
}

type respContactConnected struct {
	Contact contact `json:"contact"`
}

type respRcvFileComplete struct {
	ChatItem aChatItem `json:"chatItem"`
}

type respActiveUser struct {
	User struct {
		UserId           int64   `json:"userId"`
		LocalDisplayName string  `json:"localDisplayName"`
		Profile          profile `json:"profile"`
	} `json:"user"`
}

// ─── Command builders ───

// composedMessage is one entry in the JSON array passed to /_send.
type composedMessage struct {
	FilePath   string     `json:"filePath,omitempty"`
	MsgContent msgContent `json:"msgContent"`
}

// cmdSendText builds the command to send a text message to a contact.
func cmdSendText(contactID int64, text string) string {
	return cmdSend(contactID, "", text)
}

// cmdSendFile builds the command to send a local file (with optional caption)
// to a contact.
func cmdSendFile(contactID int64, filePath, caption string) string {
	return cmdSend(contactID, filePath, caption)
}

func cmdSend(contactID int64, filePath, text string) string {
	msgs := []composedMessage{{
		FilePath:   filePath,
		MsgContent: msgContent{Type: "text", Text: text},
	}}
	payload, _ := json.Marshal(msgs)
	return fmt.Sprintf("/_send @%d json %s", contactID, payload)
}

// cmdAcceptContact accepts an incoming contact request by its request id.
func cmdAcceptContact(reqID int64) string {
	return fmt.Sprintf("/_accept %d", reqID)
}

// cmdReceiveFile starts downloading a received file by its file id.
func cmdReceiveFile(fileID int64) string {
	return fmt.Sprintf("/freceive %d", fileID)
}

// cmdSetFilesFolder points the CLI at a directory for received files.
func cmdSetFilesFolder(path string) string {
	return "/_files_folder " + path
}

const (
	cmdShowActiveUser = "/u"
	cmdStartChat      = "/_start subscribe=on expire=off"
	cmdShowAddress    = "/show_address"
	cmdCreateAddress  = "/address"
)
