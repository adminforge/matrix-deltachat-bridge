package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

type DeltaChatBot struct {
	rpc        *deltachat.Rpc
	accId      uint32
	chatId     uint32
	adminUsers []string
	bot        *deltachat.Bot
	email      string
	inviteUrl  string
}

type DeltaChatMessage struct {
	SenderName string
	Body       string
	File       *os.File
	MsgID      uint32
	IsEdit     bool
	EditMsgID  uint32
}

type DeltaChatReaction struct {
	SenderName string
	Emoji      string
	RelatesTo  uint32
}

type NewEmailResponse struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

var safeExtRegexDC = regexp.MustCompile(`^\.[a-zA-Z0-9]+$`)

func sanitizeExtDC(ext string) string {
	ext = strings.ToLower(ext)
	if safeExtRegexDC.MatchString(ext) {
		return ext
	}
	return ".bin"
}

func isAdminDC(addr string, admins []string) bool {
	for _, a := range admins {
		if strings.EqualFold(a, addr) {
			return true
		}
	}
	return false
}

func NewDeltaChatBot(dbPath, adminList, botName string) (*DeltaChatBot, error) {
	trans := deltachat.NewIOTransport()
	if err := trans.Open(); err != nil {
		return nil, fmt.Errorf("failed to open transport: %w", err)
	}

	rpc := &deltachat.Rpc{Context: context.Background(), Transport: trans}

	// Parse admins
	var admins []string
	for _, a := range strings.Split(adminList, ",") {
		trimmed := strings.TrimSpace(a)
		if trimmed != "" {
			admins = append(admins, trimmed)
		}
	}

	accIds, err := rpc.GetAllAccountIds()
	if err != nil {
		return nil, err
	}

	var accId uint32
	var email string

	if len(accIds) == 0 {
		log.Printf("DeltaChat: No account found. Requesting new email from chat.adminforge.de...")
		resp, err := http.Get("https://chat.adminforge.de/new_email")
		if err != nil {
			return nil, fmt.Errorf("failed to request new email: %w", err)
		}
		defer resp.Body.Close()

		var newEmail NewEmailResponse
		if err := json.NewDecoder(resp.Body).Decode(&newEmail); err != nil {
			return nil, fmt.Errorf("failed to decode new email response: %w", err)
		}

		email = newEmail.Email
		password := newEmail.Password
		log.Printf("DeltaChat: Received new account: %s", email)

		accId, err = rpc.AddAccount()
		if err != nil {
			return nil, fmt.Errorf("failed to add account: %w", err)
		}

		log.Printf("DeltaChat: Configuring new account %d...", accId)
		rpc.SetConfig(accId, "addr", &email)
		rpc.SetConfig(accId, "mail_pw", &password)
		
		// Hardcode stable ports for chat.adminforge.de
		mailServer := "chat.adminforge.de"
		mailPort := "993"
		smtpPort := "465"
		secVal := "2" // SSL/TLS
		
		rpc.SetConfig(accId, "mail_server", &mailServer)
		rpc.SetConfig(accId, "mail_port", &mailPort)
		rpc.SetConfig(accId, "mail_security", &secVal)
		
		rpc.SetConfig(accId, "send_server", &mailServer)
		rpc.SetConfig(accId, "send_port", &smtpPort)
		rpc.SetConfig(accId, "send_security", &secVal)

		botVal := "1"
		rpc.SetConfig(accId, "bot", &botVal)
		
		rpc.SetConfig(accId, "e2ee_enabled", &botVal)
		rpc.SetConfig(accId, "mdns_enabled", &botVal)

		if err := rpc.Configure(accId); err != nil {
			return nil, fmt.Errorf("configuration start failed: %w", err)
		}
	} else {
		accId = accIds[0]
		addrPtr, _ := rpc.GetConfig(accId, "addr")
		if addrPtr != nil {
			email = *addrPtr
		}
		log.Printf("DeltaChat: Using existing account %d (%s)", accId, email)

		// Re-enforce stable ports even for existing accounts
		mailServer := "chat.adminforge.de"
		mailPort := "993"
		smtpPort := "465"
		secVal := "2" // SSL/TLS (Implicit)
		rpc.SetConfig(accId, "mail_server", &mailServer)
		rpc.SetConfig(accId, "mail_port", &mailPort)
		rpc.SetConfig(accId, "mail_security", &secVal)
		rpc.SetConfig(accId, "send_server", &mailServer)
		rpc.SetConfig(accId, "send_port", &smtpPort)
		rpc.SetConfig(accId, "send_security", &secVal)

		botVal := "1"
		rpc.SetConfig(accId, "bot", &botVal)
		rpc.SetConfig(accId, "e2ee_enabled", &botVal)
		rpc.SetConfig(accId, "mdns_enabled", &botVal)
		
		// Force re-configure to apply security and encryption settings
		if err := rpc.Configure(accId); err != nil {
			log.Printf("DeltaChat: Re-configure failed: %v", err)
		}
	}

	rpc.SetConfig(accId, "displayname", &botName)

	log.Printf("DeltaChat: Waiting for account to be ready...")
	configured := false
	for i := 0; i < 60; i++ {
		ok, err := rpc.IsConfigured(accId)
		if err == nil && ok {
			configured = true
			break
		}
		time.Sleep(1 * time.Second)
	}

	if !configured {
		return nil, fmt.Errorf("account configuration timed out")
	}

	log.Printf("DeltaChat: Account %s is ready!", email)

	// Ensure admin contacts are created
	for _, adminAddr := range admins {
		emptyName := ""
		_, err := rpc.CreateContact(accId, adminAddr, &emptyName)
		if err != nil {
			log.Printf("DeltaChat: Warning: Could not create admin contact %s: %v", adminAddr, err)
		}
	}

	inviteUrl, _ := rpc.GetChatSecurejoinQrCode(accId, nil)

	bot := deltachat.NewBot(rpc)

	dcBot := &DeltaChatBot{
		rpc:        rpc,
		accId:      accId,
		adminUsers: admins,
		bot:        bot,
		email:      email,
		inviteUrl:  inviteUrl,
	}

	return dcBot, nil
}

func (b *DeltaChatBot) Start(msgChan chan<- DeltaChatMessage, reactChan chan<- DeltaChatReaction, onJoin func(uint32)) {
	b.bot.OnNewMsg(func(bot *deltachat.Bot, accId uint32, msgId uint32) {
		if accId != b.accId {
			return
		}

		msg, err := bot.Rpc.GetMessage(accId, msgId)
		if err != nil {
			return
		}

		if msg.FromId == deltachat.ContactSelf {
			return
		}

		isEdit := msg.IsEdited && msg.OriginalMsgId != nil
		var editMsgID uint32
		if isEdit {
			editMsgID = *msg.OriginalMsgId
		}

		if msg.State >= 200 || (msg.IsInfo && !isEdit) {
			return
		}

		if msg.ViewType == deltachat.ViewtypeUnknown && !isEdit {
			return
		}

		// Resolve sender address for relaying
		senderAddr := "Unknown"
		contact, err := bot.Rpc.GetContact(accId, msg.FromId)
		senderName := "Unknown"
		if err == nil {
			senderAddr = contact.Address
			senderName = contact.DisplayName
			if senderName == "" {
				senderName = contact.Address
			}
		}

		isFromAdmin := isAdminDC(senderAddr, b.adminUsers)

		chatInfo, err := bot.Rpc.GetBasicChatInfo(accId, msg.ChatId)
		if err != nil {
			return
		}

		// COMMAND HANDLING
		if isFromAdmin && !isEdit {
			if chatInfo.ChatType == deltachat.ChatTypeSingle || strings.HasPrefix(msg.Text, "/") {
				b.handleCommand(msg.ChatId, msg.Text, onJoin)
				return
			}
		}

		// RELAYING
		if msg.ChatId == b.chatId {
			if msg.File != nil && !isEdit {
				tmpFile, err := os.CreateTemp("", "dc-bridge-*")
				if err == nil {
					ext := sanitizeExtDC(filepath.Ext(*msg.File))
					tmpFilePath := tmpFile.Name() + ext
					tmpFile.Close()
					
					err = b.rpc.SaveMsgFile(accId, msgId, tmpFilePath)
					if err == nil {
						finalFile, _ := os.Open(tmpFilePath)
						msgChan <- DeltaChatMessage{
							SenderName: senderName,
							Body:       msg.Text,
							File:       finalFile,
							MsgID:      msgId,
							IsEdit:     isEdit,
							EditMsgID:  editMsgID,
						}
						return
					}
					os.Remove(tmpFilePath)
				}
			}

			if msg.Text != "" || isEdit {
				msgChan <- DeltaChatMessage{
					SenderName: senderName,
					Body:       msg.Text,
					MsgID:      msgId,
					IsEdit:     isEdit,
					EditMsgID:  editMsgID,
				}
			}
		}
	})

	// Handle Reactions
	b.bot.On(&deltachat.EventTypeIncomingReaction{}, func(bot *deltachat.Bot, accId uint32, event deltachat.EventType) {
		if accId != b.accId {
			return
		}
		ev := event.(*deltachat.EventTypeIncomingReaction)
		if ev.ChatId != b.chatId {
			return
		}
		
		contact, err := bot.Rpc.GetContact(accId, ev.ContactId)
		senderName := "Unknown"
		if err == nil {
			senderName = contact.DisplayName
			if senderName == "" {
				senderName = contact.Address
			}
		}

		reactChan <- DeltaChatReaction{
			SenderName: senderName,
			Emoji:      ev.Reaction,
			RelatesTo:  ev.MsgId,
		}
	})

	log.Printf("DeltaChat: Bot loop running...")
	b.bot.Run()
}

func (b *DeltaChatBot) handleCommand(chatId uint32, body string, onJoin func(uint32)) {
	parts := strings.Fields(body)
	cmd := ""
	if len(parts) > 0 {
		cmd = parts[0]
	}

	chatInfo, _ := b.rpc.GetBasicChatInfo(b.accId, chatId)

	if cmd == "/set" {
		if chatInfo.ChatType != deltachat.ChatTypeGroup {
			b.rpc.MiscSendTextMessage(b.accId, chatId, "❌ `/set` geht nur in Gruppen.")
			return
		}
		b.chatId = chatId
		b.rpc.MiscSendTextMessage(b.accId, chatId, fmt.Sprintf("✅ DeltaChat-Brücke auf diese Gruppe (ID: %d) gesetzt.", chatId))
		if onJoin != nil {
			onJoin(chatId)
		}
		return
	}

	if cmd == "/join" {
		if len(parts) < 2 {
			b.rpc.MiscSendTextMessage(b.accId, chatId, "❌ Bitte Link angeben: `/join https://i.delta.chat/...`")
			return
		}
		link := parts[1]
		b.rpc.MiscSendTextMessage(b.accId, chatId, "⏳ Trete bei...")
		newChatId, err := b.rpc.SecureJoin(b.accId, link)
		if err != nil {
			b.rpc.MiscSendTextMessage(b.accId, chatId, fmt.Sprintf("❌ Fehler: %v", err))
			return
		}
		b.chatId = newChatId
		b.rpc.MiscSendTextMessage(b.accId, chatId, fmt.Sprintf("✅ DeltaChat-Brücke gesetzt auf Chat ID: %d", newChatId))
		if onJoin != nil {
			onJoin(newChatId)
		}
		return
	}

	if chatInfo.ChatType == deltachat.ChatTypeSingle {
		entries, _ := b.rpc.GetChatlistEntries(b.accId, nil, nil, nil)
		chatList := ""
		for _, cid := range entries {
			info, err := b.rpc.GetBasicChatInfo(b.accId, cid)
			if err == nil && !info.IsSelfTalk {
				chatType := "👤"
				if info.ChatType == deltachat.ChatTypeGroup {
					chatType = "👥"
				}
				chatList += fmt.Sprintf("%s %s (ID: `%d`)\n", chatType, info.Name, cid)
			}
		}

		statusMsg := fmt.Sprintf("👋 Hallo Admin! Du bist berechtigt, mich in Gruppen einzuladen.\n\n"+
			"**Aktueller Status:**\n"+
			"• Bot Email: `%s`\n\n"+
			"**Ich bin in folgenden Chats:**\n%s\n"+
			"• Nutze `/set` in einer Gruppe um sie als Brücke zu setzen.", b.email, chatList)
		
		b.rpc.MiscSendTextMessage(b.accId, chatId, statusMsg)
	}
}

func (b *DeltaChatBot) SetBridgedChat(chatId uint32) {
	b.chatId = chatId
}

func (b *DeltaChatBot) EditMessage(msgId uint32, text string) {
	if b.chatId == 0 || msgId == 0 {
		return
	}
	err := b.rpc.SendEditRequest(b.accId, msgId, text)
	if err != nil {
		log.Printf("DeltaChat: Error editing message %d: %v", msgId, err)
	}
}

func (b *DeltaChatBot) SendMessage(text string) uint32 {
	if b.chatId == 0 {
		return 0
	}
	msgId, err := b.rpc.MiscSendTextMessage(b.accId, b.chatId, text)
	if err != nil {
		return 0
	}
	return msgId
}

func (b *DeltaChatBot) SendMedia(text string, filePath string) uint32 {
	if b.chatId == 0 {
		return 0
	}
	
	ext := strings.ToLower(filepath.Ext(filePath))
	viewType := deltachat.ViewtypeFile
	switch ext {
	case ".jpg", ".jpeg", ".png":
		viewType = deltachat.ViewtypeImage
	case ".gif":
		viewType = deltachat.ViewtypeGif
	case ".mp4", ".mov", ".avi":
		viewType = deltachat.ViewtypeVideo
	case ".mp3", ".ogg", ".wav":
		viewType = deltachat.ViewtypeAudio
	}

	filename := filepath.Base(filePath)
	pair, err := b.rpc.MiscSendMsg(b.accId, b.chatId, &text, &filePath, &filename, nil, nil)
	if err != nil {
		return 0
	}
	
	msgId, err := b.rpc.SendMsg(b.accId, b.chatId, deltachat.MessageData{
		Text:     &text,
		File:     &filePath,
		Filename: &filename,
		Viewtype: &viewType,
	})
	if err != nil {
		return pair.First
	}
	return msgId
}

func (b *DeltaChatBot) React(msgId uint32, emoji string) error {
	if b.chatId == 0 || msgId == 0 {
		return nil
	}
	_, err := b.rpc.SendReaction(b.accId, msgId, []string{emoji})
	return err
}
