package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Config struct {
	MatrixRoom     id.RoomID `json:"matrix_room"`
	DCChatID       uint32    `json:"dc_chat_id"`
	MatrixDeviceID id.DeviceID `json:"matrix_device_id"`
}

var (
	mappingDB *sql.DB
	lastMessages = make(map[string]time.Time)
	cacheMutex   sync.Mutex
)

func initMappingDB() {
	var err error
	mappingDB, err = sql.Open("sqlite3", "/data/bridge_mapping.db")
	if err != nil {
		log.Fatal(err)
	}
	_, err = mappingDB.Exec("CREATE TABLE IF NOT EXISTS mapping (matrix_id TEXT PRIMARY KEY, dc_id INTEGER)")
	if err != nil {
		log.Fatal(err)
	}
}

func storeMapping(matrixID id.EventID, dcID uint32) {
	_, err := mappingDB.Exec("INSERT OR REPLACE INTO mapping (matrix_id, dc_id) VALUES (?, ?)", string(matrixID), dcID)
	if err != nil {
		log.Printf("Bridge: Error storing mapping: %v", err)
	}
}

func getDCID(matrixID id.EventID) uint32 {
	var dcID uint32
	err := mappingDB.QueryRow("SELECT dc_id FROM mapping WHERE matrix_id = ?", string(matrixID)).Scan(&dcID)
	if err != nil {
		return 0
	}
	return dcID
}

func getMatrixID(dcID uint32) id.EventID {
	var matrixID string
	err := mappingDB.QueryRow("SELECT matrix_id FROM mapping WHERE dc_id = ?", dcID).Scan(&matrixID)
	if err != nil {
		return ""
	}
	return id.EventID(matrixID)
}

func loadConfig() Config {
	file, err := os.ReadFile("/data/bridge_config.json")
	if err != nil {
		return Config{}
	}
	var cfg Config
	json.Unmarshal(file, &cfg)
	return cfg
}

func saveConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile("/data/bridge_config.json", data, 0644)
}

// deduplication helper
func isDuplicate(key string) bool {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	if lastTime, ok := lastMessages[key]; ok {
		if time.Since(lastTime) < 10*time.Second {
			return true
		}
	}

	// Cleanup old entries
	if len(lastMessages) > 200 {
		for k, t := range lastMessages {
			if time.Since(t) > 1*time.Minute {
				delete(lastMessages, k)
			}
		}
	}

	lastMessages[key] = time.Now()
	return false
}

func stripMatrixUser(mxid id.UserID) string {
	user := strings.TrimPrefix(string(mxid), "@")
	parts := strings.Split(user, ":")
	return parts[0]
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using environment variables")
	}

	initMappingDB()

	matrixHS := os.Getenv("MATRIX_HOMESERVER")
	matrixAdmin := os.Getenv("MATRIX_ADMIN")
	matrixBotName := os.Getenv("MATRIX_BOT_NAME")
	if matrixBotName == "" {
		matrixBotName = "matrixdeltabot"
	}

	dcAdmin := os.Getenv("DELTACHAT_ADMIN")
	dcBotName := os.Getenv("DELTACHAT_BOT_NAME")
	if dcBotName == "" {
		dcBotName = "matrixdeltabot"
	}

	matrixUser := os.Getenv("MATRIX_USER")
	matrixPass := os.Getenv("MATRIX_PASSWORD")

	if matrixHS == "" || matrixAdmin == "" || dcAdmin == "" {
		log.Fatal("Missing required environment variables.")
	}

	dbPath := "/data/deltachat.db"
	cfg := loadConfig()

	// --- Initialization loop with smart recovery ---
	var mBot *MatrixBot
	backoff := 60
	for {
		log.Println("Bridge: Initializing Matrix bot...")
		mBot, err = NewMatrixBot(matrixHS, matrixAdmin, matrixBotName, matrixUser, matrixPass, cfg.MatrixDeviceID)
		if err != nil {
			if strings.Contains(err.Error(), "CRYPTO_ERROR_RETRY_NEEDED") {
				log.Println("Bridge: Triggering fresh start due to crypto mismatch...")
				cfg.MatrixDeviceID = "" // Wipe stored ID
				saveConfig(cfg)
				continue // Retry immediately without DeviceID
			}
			
			if strings.Contains(err.Error(), "429") {
				log.Printf("Bridge: Too many requests. Waiting %ds...", backoff)
				time.Sleep(time.Duration(backoff) * time.Second)
				if backoff < 300 {
					backoff += 60 // Linear increase up to 5 mins
				}
				continue
			}

			log.Fatalf("Failed to initialize Matrix bot: %v", err)
		}
		break
	}
	
	if mBot.client.DeviceID != cfg.MatrixDeviceID {
		cfg.MatrixDeviceID = mBot.client.DeviceID
		saveConfig(cfg)
	}

	if cfg.MatrixRoom != "" {
		mBot.SetBridgedRoom(cfg.MatrixRoom)
	}

	log.Println("Bridge: Initializing Delta Chat bot...")
	var dBot *DeltaChatBot
	for {
		dBot, err = NewDeltaChatBot(dbPath, dcAdmin, dcBotName)
		if err != nil {
			log.Printf("Bridge: Failed to initialize Delta Chat bot: %v. Retrying in 60s...", err)
			time.Sleep(60 * time.Second)
			continue
		}
		break
	}

	if cfg.DCChatID != 0 {
		dBot.SetBridgedChat(cfg.DCChatID)
	}

	log.Printf("================================================")
	log.Printf("BRIDGE BOT IDENTITIES:")
	log.Printf("Matrix User: %s", mBot.userId)
	log.Printf("Matrix Invite: https://matrix.to/#/%s", mBot.userId)
	log.Printf("DeltaChat Email: %s", dBot.email)
	if dBot.inviteUrl != "" {
		log.Printf("DeltaChat Invite Link:")
		log.Printf(">>> %s <<<", dBot.inviteUrl)
	}
	log.Printf("================================================")

	matrixToDcChan := make(chan MatrixMessage, 100)
	matrixReactChan := make(chan MatrixReaction, 100)
	dcToMatrixChan := make(chan DeltaChatMessage, 100)
	dcReactChan := make(chan DeltaChatReaction, 100)

	// Forward Matrix -> Delta Chat Messages
	go func() {
		for msg := range matrixToDcChan {
			// Use EventID for 100% reliable deduplication from Matrix
			if isDuplicate("m_" + string(msg.EventID)) {
				continue
			}
			
			cleanUser := stripMatrixUser(id.UserID(msg.Sender))
			formatted := fmt.Sprintf("[matrix] %s", cleanUser)
			
			// For media, check if body is just a filename or a real caption
			isCaption := false
			if msg.Body != "" {
				ext := strings.ToLower(filepath.Ext(msg.Body))
				isGenericFile := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".mp4" || ext == ".pdf"
				
				if !isGenericFile || strings.Contains(msg.Body, " ") {
					formatted = fmt.Sprintf("[matrix] %s: %s", cleanUser, msg.Body)
					isCaption = true
				}
			}

			if msg.IsEdit {
				dcID := getDCID(msg.EditEventID)
				if dcID != 0 {
					dBot.EditMessage(dcID, formatted+" (bearbeitet)")
				}
				continue
			}

			var dcID uint32
			replyToDC := uint32(0)
			if msg.ReplyTo != "" {
				replyToDC = getDCID(msg.ReplyTo)
			}

			if msg.File != nil {
				// If it wasn't a real caption, just use the sender info for the media text
				if !isCaption {
					formatted = fmt.Sprintf("[matrix] %s", cleanUser)
				}
				
				if replyToDC != 0 {
					dcID = dBot.SendMediaWithReply(formatted, msg.File.Name(), replyToDC)
				} else {
					dcID = dBot.SendMedia(formatted, msg.File.Name())
				}
				msg.File.Close()
				os.Remove(msg.File.Name())
			} else {
				// For text messages, always use the formatted body with colon
				formatted = fmt.Sprintf("[matrix] %s: %s", cleanUser, msg.Body)
				if replyToDC != 0 {
					dcID = dBot.SendMessageWithReply(formatted, replyToDC)
				} else {
					dcID = dBot.SendMessage(formatted)
				}
			}
			if dcID != 0 {
				storeMapping(msg.EventID, dcID)
			}
		}
	}()

	// Matrix -> Delta Chat Reactions
	go func() {
		for react := range matrixReactChan {
			dcID := getDCID(react.RelatesTo)
			if dcID != 0 {
				err := dBot.React(dcID, react.Emoji)
				if err != nil {
					log.Printf("Bridge: Warning: Error sending reaction to DC: %v", err)
				}
			}
		}
	}()

	// Forward Delta Chat -> Matrix Messages
	go func() {
		for msg := range dcToMatrixChan {
			// Use MsgID for reliable deduplication from Delta Chat
			if isDuplicate(fmt.Sprintf("dc_%d", msg.MsgID)) {
				continue
			}

			if msg.IsEdit {
				mEventID := getMatrixID(msg.EditMsgID)
				if mEventID != "" {
					formatted := fmt.Sprintf("[deltachat] %s: %s", msg.SenderName, msg.Body)
					mBot.EditMessage(mEventID, formatted+" (bearbeitet)")
				}
				continue
			}

			var mEventID id.EventID
			replyToMatrix := id.EventID("")
			if msg.ReplyTo != 0 {
				replyToMatrix = getMatrixID(msg.ReplyTo)
			}

			if msg.File != nil {
				mType := event.MsgFile
				ext := strings.ToLower(filepath.Ext(msg.File.Name()))
				switch ext {
				case ".mp4", ".mov", ".avi", ".webm", ".mkv", ".m4v", ".3gp": 
					mType = event.MsgVideo
				case ".mp3", ".ogg", ".wav", ".m4a", ".aac", ".flac", ".opus": 
					mType = event.MsgAudio
				case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tiff", ".heic", ".heif":
					mType = event.MsgImage
				default:
					mType = event.MsgFile
				}
				
				formatted := fmt.Sprintf("[deltachat] %s", msg.SenderName)
				// If the body is just a generic DC placeholder, don't include it in the Matrix caption to avoid dual-captions
				if msg.Body != "" && !strings.HasPrefix(msg.Body, "[Image ") && !strings.HasPrefix(msg.Body, "[Video ") && !strings.HasPrefix(msg.Body, "[File ") {
					formatted = fmt.Sprintf("[deltachat] %s: %s", msg.SenderName, msg.Body)
				}
				
				if replyToMatrix != "" {
					mEventID = mBot.SendMediaWithReply(formatted, msg.File.Name(), mType, msg.FileMime, replyToMatrix)
				} else {
					mEventID = mBot.SendMedia(formatted, msg.File.Name(), mType, msg.FileMime)
				}

				msg.File.Close()
				os.Remove(msg.File.Name())
			} else {
				formatted := fmt.Sprintf("[deltachat] %s: %s", msg.SenderName, msg.Body)
				if replyToMatrix != "" {
					mEventID = mBot.SendMessageWithReply(formatted, replyToMatrix)
				} else {
					mEventID = mBot.SendMessage(formatted)
				}
			}
			if mEventID != "" {
				storeMapping(mEventID, msg.MsgID)
			}
		}
	}()

	// Delta Chat -> Matrix Reactions
	go func() {
		for react := range dcReactChan {
			mEventID := getMatrixID(react.RelatesTo)
			if mEventID != "" {
				emojis := strings.Fields(react.Emoji)
				for _, emoji := range emojis {
					mBot.React(mEventID, emoji)
				}
			}
		}
	}()

	// Start Matrix event loop in goroutine
	go func() {
		err := mBot.Start(matrixToDcChan, matrixReactChan, func(roomId id.RoomID) {
			cfg.MatrixRoom = roomId
			saveConfig(cfg)
			log.Printf("Bridge: Matrix Room ID updated: %s", roomId)
		})
		if err != nil {
			log.Fatalf("Matrix bot loop failed: %v", err)
		}
	}()

	// Start Delta Chat event loop in goroutine
	go func() {
		dBot.Start(dcToMatrixChan, dcReactChan, func(chatId uint32) {
			cfg.DCChatID = chatId
			saveConfig(cfg)
			log.Printf("Bridge: Delta Chat ID updated: %d", chatId)
		})
	}()

	time.Sleep(2 * time.Second)
	log.Println("Bridge: Running.")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Bridge: Shutting down...")
}
