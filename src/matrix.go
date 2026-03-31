package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type MatrixBot struct {
	client       *mautrix.Client
	bridgedRoom  id.RoomID
	adminUsers   []id.UserID
	userId       id.UserID
	cryptoHelper *cryptohelper.CryptoHelper
	processed    map[id.EventID]bool
	procMutex    sync.Mutex
}

func (b *MatrixBot) isProcessed(id id.EventID) bool {
	b.procMutex.Lock()
	defer b.procMutex.Unlock()
	if b.processed[id] {
		return true
	}
	if len(b.processed) > 500 {
		for k := range b.processed {
			delete(b.processed, k)
			break
		}
	}
	b.processed[id] = true
	return false
}

type MatrixMessage struct {
	Sender      string
	Body        string
	File        *os.File
	Type        event.MessageType
	EventID     id.EventID
	IsEdit      bool
	EditEventID id.EventID
	ReplyTo     id.EventID
}

type MatrixReaction struct {
	Sender    string
	Emoji     string
	RelatesTo id.EventID
}

var safeExtRegex = regexp.MustCompile(`^\.[a-zA-Z0-9]+$`)

func sanitizeExt(ext string) string {
	ext = strings.ToLower(ext)
	if safeExtRegex.MatchString(ext) {
		return ext
	}
	return ".bin"
}

func generateRandomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func isAdminMatrix(user id.UserID, admins []id.UserID) bool {
	for _, a := range admins {
		if a == user {
			return true
		}
	}
	return false
}

func NewMatrixBot(homeserver, adminList, botName, envUser, envPass string, existingDeviceID id.DeviceID) (*MatrixBot, error) {
	client, err := mautrix.NewClient(homeserver, "", "")
	if err != nil {
		return nil, err
	}

	var admins []id.UserID
	for _, a := range strings.Split(adminList, ",") {
		trimmed := id.UserID(strings.TrimSpace(a))
		if trimmed != "" {
			admins = append(admins, trimmed)
		}
	}

	ctx := context.Background()
	var userId id.UserID
	var accessToken string
	var deviceId id.DeviceID

	// 1. LOGIN
	log.Printf("Matrix: Attempting login as %s...", envUser)
	req := &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{
			Type: mautrix.IdentifierTypeUser,
			User: envUser,
		},
		Password:                 envPass,
		InitialDeviceDisplayName: "Bridge Bot",
	}
	if existingDeviceID != "" {
		req.DeviceID = existingDeviceID
	}
	
	resp, err := client.Login(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "M_LIMIT_EXCEEDED") {
			return nil, fmt.Errorf("rate limited by server, please wait a few minutes (429)")
		}
		return nil, fmt.Errorf("login failed: %w", err)
	}
	userId = resp.UserID
	accessToken = resp.AccessToken
	deviceId = resp.DeviceID

	client.SetCredentials(userId, accessToken)
	client.UserID = userId
	client.AccessToken = accessToken
	client.DeviceID = deviceId
	
	_ = client.SetDisplayName(ctx, botName)

	// 2. CRYPTO
	cryptoDBPath := "/data/matrix_crypto.db"
	
	initCrypto := func() (*cryptohelper.CryptoHelper, error) {
		rawDb, err := sql.Open("sqlite3", "file:"+cryptoDBPath+"?cache=shared&_ext=1")
		if err != nil {
			return nil, fmt.Errorf("failed to open crypto database: %w", err)
		}
		db, err := dbutil.NewWithDB(rawDb, "sqlite3")
		if err != nil {
			return nil, fmt.Errorf("failed to create dbutil wrapper: %w", err)
		}

		pickleKey := []byte("matrix-bridge-pickle-key-1234567890")
		helper, err := cryptohelper.NewCryptoHelper(client, pickleKey, db)
		if err != nil {
			return nil, fmt.Errorf("failed to create crypto helper: %w", err)
		}

		helper.DBAccountID = "bridge-bot"
		err = helper.Init(ctx)
		return helper, err
	}

	helper, err := initCrypto()
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "mismatching device ID") || 
		   strings.Contains(errMsg, "not marked as shared") ||
		   strings.Contains(errMsg, "keys seem to have disappeared") {
			
			log.Printf("Matrix: Crypto corruption/mismatch detected (%s). PURGING and requesting fresh start...", errMsg)
			os.Remove(cryptoDBPath)
			os.Remove(cryptoDBPath + "-shm")
			os.Remove(cryptoDBPath + "-wal")
			return nil, fmt.Errorf("CRYPTO_ERROR_RETRY_NEEDED: %s", errMsg)
		}
		return nil, fmt.Errorf("failed to initialize crypto helper: %w", err)
	}

	bot := &MatrixBot{
		client:       client,
		adminUsers:   admins,
		userId:       userId,
		cryptoHelper: helper,
		processed:    make(map[id.EventID]bool),
	}

	return bot, nil
}

func (b *MatrixBot) Start(msgChan chan<- MatrixMessage, reactChan chan<- MatrixReaction, onJoin func(id.RoomID)) error {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	
	syncer.OnEventType(event.StateMember, b.cryptoHelper.Machine().HandleMemberEvent)
	syncer.OnEventType(event.EventEncrypted, b.cryptoHelper.HandleEncrypted)

	syncer.OnEventType(event.StateMember, func(ctx context.Context, ev *event.Event) {
		if ev.GetStateKey() == string(b.userId) {
			content := ev.Content.AsMember()
			if content.Membership == event.MembershipInvite {
				if isAdminMatrix(ev.Sender, b.adminUsers) {
					log.Printf("Matrix: Received authorized invite to %s. Joining...", ev.RoomID)
					_, err := b.client.JoinRoom(ctx, string(ev.RoomID), nil)
					if err == nil {
						log.Printf("Matrix: Successfully joined %s", ev.RoomID)
					}
				} else {
					log.Printf("Matrix: Received unauthorized invite from %s to %s. Ignoring.", ev.Sender, ev.RoomID)
				}
			}
		}
	})

	// Handle Reactions
	syncer.OnEventType(event.EventReaction, func(ctx context.Context, ev *event.Event) {
		if ev.Sender == b.userId || ev.RoomID != b.bridgedRoom || b.isProcessed(ev.ID) {
			return
		}
		content := ev.Content.AsReaction()
		// In ReactionEventContent, RelatesTo is a struct, not a pointer.
		if content != nil && content.RelatesTo.EventID != "" && content.RelatesTo.Type == event.RelAnnotation {
			reactChan <- MatrixReaction{
				Sender:    string(ev.Sender),
				Emoji:     content.RelatesTo.Key,
				RelatesTo: content.RelatesTo.EventID,
			}
		}
	})

	syncer.OnEvent(func(ctx context.Context, ev *event.Event) {
		if ev.Sender == b.userId {
			return
		}

		if ev.Type != event.EventMessage {
			return
		}

		if b.isProcessed(ev.ID) {
			return
		}

		content := ev.Content.AsMessage()
		if content == nil {
			return
		}

		body := strings.TrimSpace(content.Body)
		isEdit := false
		var editID id.EventID
		var replyTo id.EventID

		// Handle Edits/Replies (RelatesTo in MessageEventContent is a pointer)
		if content.RelatesTo != nil {
			if content.RelatesTo.Type == event.RelReplace && content.RelatesTo.EventID != "" {
				isEdit = true
				editID = content.RelatesTo.EventID
				if content.NewContent != nil {
					body = strings.TrimSpace(content.NewContent.Body)
				}
			}
			if content.RelatesTo.InReplyTo != nil {
				replyTo = content.RelatesTo.InReplyTo.EventID
			}
		}

		// Check for Admin Commands
		if isAdminMatrix(ev.Sender, b.adminUsers) {
			if ev.RoomID != b.bridgedRoom || strings.HasPrefix(body, "/") {
				b.handleCommand(ctx, ev.RoomID, body, onJoin)
				return
			}
		}

		if ev.RoomID == b.bridgedRoom {
			switch content.MsgType {
			case event.MsgText, event.MsgEmote:
				msgChan <- MatrixMessage{
					Sender:      string(ev.Sender),
					Body:        body,
					Type:        content.MsgType,
					EventID:     ev.ID,
					IsEdit:      isEdit,
					EditEventID: editID,
					ReplyTo:     replyTo,
				}
			
			case event.MsgImage, event.MsgVideo, event.MsgFile, event.MsgAudio:
				file, err := b.downloadMedia(ctx, content)
				if err != nil {
					log.Printf("Matrix: Failed to download media: %v", err)
					return
				}
				msgChan <- MatrixMessage{
					Sender:      string(ev.Sender),
					Body:        body,
					File:        file,
					Type:        content.MsgType,
					EventID:     ev.ID,
					IsEdit:      isEdit,
					EditEventID: editID,
					ReplyTo:     replyTo,
				}
			}
		}
	})

	return b.client.Sync()
}

func (b *MatrixBot) downloadMedia(ctx context.Context, content *event.MessageEventContent) (*os.File, error) {
	var mxc id.ContentURI
	if content.File != nil {
		mxc = content.File.URL.ParseOrIgnore()
	} else {
		mxc = content.URL.ParseOrIgnore()
	}
	
	resp, err := b.client.Download(ctx, mxc)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	ext := ""
	if content.Info != nil && content.Info.MimeType != "" {
		parts := strings.Split(content.Info.MimeType, "/")
		if len(parts) == 2 {
			ext = sanitizeExt("." + parts[1])
		}
	}
	if ext == "" {
		ext = sanitizeExt(filepath.Ext(content.Body))
	}

	tmpFile, err := os.CreateTemp("", "matrix-bridge-*"+ext)
	if err != nil {
		return nil, err
	}

	if content.File != nil {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		decrypted, err := content.File.Decrypt(data)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(tmpFile, bytes.NewReader(decrypted))
		if err != nil {
			return nil, err
		}
	} else {
		_, err = io.Copy(tmpFile, resp.Body)
		if err != nil {
			return nil, err
		}
	}

	tmpFile.Seek(0, 0)
	return tmpFile, nil
}

func (b *MatrixBot) handleCommand(ctx context.Context, roomId id.RoomID, body string, onJoin func(id.RoomID)) {
	parts := strings.Fields(body)
	cmd := ""
	if len(parts) > 0 {
		cmd = parts[0]
	}

	if cmd == "/set" {
		target := string(roomId)
		b.bridgedRoom = id.RoomID(target)
		b.sendMessage(ctx, roomId, fmt.Sprintf("✅ Matrix-Brücke gesetzt auf diesen Raum: %s", target))
		if onJoin != nil {
			onJoin(b.bridgedRoom)
		}
		return
	}

	members, err := b.client.JoinedMembers(ctx, roomId)
	isDM := err == nil && len(members.Joined) <= 2

	if isDM {
		joinedRooms, err := b.client.JoinedRooms(ctx)
		roomList := ""
		if err == nil {
			for _, rid := range joinedRooms.JoinedRooms {
				roomList += fmt.Sprintf("• `%s`\n", rid)
			}
		}

		statusMsg := fmt.Sprintf("👋 Hallo Admin!\n\n"+
			"**Aktueller Status:**\n"+
			"• Bot ID: `%s`\n\n"+
			"**Ich bin in folgenden Räumen:**\n%s\n"+
			"Nutze `/set` in einem Raum um ihn als Brücke zu setzen.", b.userId, roomList)
		
		b.sendMessage(ctx, roomId, statusMsg)
	}
}

func (b *MatrixBot) sendMessage(ctx context.Context, roomId id.RoomID, text string) id.EventID {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}
	resp, err := b.client.SendMessageEvent(ctx, roomId, event.EventMessage, content)
	if err != nil {
		log.Printf("Matrix: Error sending message to %s: %v", roomId, err)
		return ""
	}
	return resp.EventID
}

func (b *MatrixBot) SendMessage(text string) id.EventID {
	if b.bridgedRoom == "" {
		return ""
	}
	return b.sendMessage(context.Background(), b.bridgedRoom, text)
}

func (b *MatrixBot) SendMessageWithReply(text string, replyTo id.EventID) id.EventID {
	if b.bridgedRoom == "" {
		return ""
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
		RelatesTo: &event.RelatesTo{
			InReplyTo: &event.InReplyTo{
				EventID: replyTo,
			},
		},
	}
	resp, err := b.client.SendMessageEvent(context.Background(), b.bridgedRoom, event.EventMessage, content)
	if err != nil {
		log.Printf("Matrix: Error sending reply to %s: %v", b.bridgedRoom, err)
		return ""
	}
	return resp.EventID
}

func (b *MatrixBot) SendMedia(text string, filePath string, msgType event.MessageType) id.EventID {
	return b.SendMediaWithReply(text, filePath, msgType, "")
}

func (b *MatrixBot) SendMediaWithReply(text string, filePath string, msgType event.MessageType, replyTo id.EventID) id.EventID {
	if b.bridgedRoom == "" {
		return ""
	}
	
	ctx := context.Background()
	file, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer file.Close()

	stat, _ := file.Stat()
	mime := "application/octet-stream"
	ext := strings.ToLower(filepath.Ext(filePath))
	
	var width, height int
	if msgType == event.MsgImage {
		if cfg, _, err := image.DecodeConfig(file); err == nil {
			width = cfg.Width
			height = cfg.Height
		}
		file.Seek(0, 0)
	}

	switch ext {
	case ".jpg", ".jpeg": mime = "image/jpeg"
	case ".png": mime = "image/png"
	case ".gif": mime = "image/gif"
	case ".mp4": mime = "video/mp4"
	}

	resp, err := b.client.UploadMedia(ctx, mautrix.ReqUploadMedia{
		Content:       file,
		ContentLength: stat.Size(),
		ContentType:   mime,
		FileName:      filepath.Base(filePath),
	})
	if err != nil {
		return ""
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    filepath.Base(filePath),
		URL:     resp.ContentURI.CUString(),
		Info: &event.FileInfo{
			Size:     int(stat.Size()),
			MimeType: mime,
			Width:    width,
			Height:   height,
		},
	}

	if replyTo != "" {
		content.RelatesTo = &event.RelatesTo{
			InReplyTo: &event.InReplyTo{
				EventID: replyTo,
			},
		}
	}

	respMsg, err := b.client.SendMessageEvent(ctx, b.bridgedRoom, event.EventMessage, content)
	if err != nil {
		return ""
	}
	return respMsg.EventID
}

func (b *MatrixBot) React(eventID id.EventID, emoji string) id.EventID {
	if b.bridgedRoom == "" || eventID == "" {
		return ""
	}
	content := &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{
			Type:    event.RelAnnotation,
			EventID: eventID,
			Key:     emoji,
		},
	}
	resp, err := b.client.SendMessageEvent(context.Background(), b.bridgedRoom, event.EventReaction, content)
	if err != nil {
		return ""
	}
	return resp.EventID
}

func (b *MatrixBot) EditMessage(originalEventID id.EventID, text string) id.EventID {
	if b.bridgedRoom == "" || originalEventID == "" {
		return ""
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "* " + text,
		RelatesTo: &event.RelatesTo{
			Type:    event.RelReplace,
			EventID: originalEventID,
		},
		NewContent: &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    text,
		},
	}
	resp, err := b.client.SendMessageEvent(context.Background(), b.bridgedRoom, event.EventMessage, content)
	if err != nil {
		log.Printf("Matrix: Error editing message %s: %v", originalEventID, err)
		return ""
	}
	return resp.EventID
}

func (b *MatrixBot) SetBridgedRoom(room id.RoomID) {
	b.bridgedRoom = room
}
