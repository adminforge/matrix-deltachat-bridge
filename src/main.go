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
	MatrixRoom     id.RoomID   `json:"matrix_room"`
	MatrixDeviceID id.DeviceID `json:"matrix_device_id"`
	DCChatID       uint32      `json:"dc_chat_id"`
}

const configPath = "/data/bridge_config.json"
const mappingDBPath = "/data/bridge_mapping.db"

func loadConfig() Config {
	var cfg Config
	file, err := os.Open(configPath)
	if err == nil {
		json.NewDecoder(file).Decode(&cfg)
		file.Close()
	}
	return cfg
}

func saveConfig(cfg Config) {
	file, err := os.Create(configPath)
	if err == nil {
		json.NewEncoder(file).Encode(cfg)
		file.Close()
	}
}

var mappingDB *sql.DB

func initMappingDB() {
	var err error
	mappingDB, err = sql.Open("sqlite3", mappingDBPath)
	if err != nil {
		log.Fatalf("Failed to open mapping DB: %v", err)
	}
	_, err = mappingDB.Exec(`CREATE TABLE IF NOT EXISTS msg_mapping (
		matrix_id TEXT PRIMARY KEY,
		dc_id INTEGER
	)`)
	if err != nil {
		log.Fatalf("Failed to create mapping table: %v", err)
	}
	_, err = mappingDB.Exec(`CREATE INDEX IF NOT EXISTS idx_dc_id ON msg_mapping(dc_id)`)
}

func storeMapping(matrixID id.EventID, dcID uint32) {
	if matrixID == "" || dcID == 0 {
		return
	}
	mappingDB.Exec("INSERT OR REPLACE INTO msg_mapping (matrix_id, dc_id) VALUES (?, ?)", string(matrixID), dcID)
}

func getDCID(matrixID id.EventID) uint32 {
	var dcID uint32
	err := mappingDB.QueryRow("SELECT dc_id FROM msg_mapping WHERE matrix_id = ?", string(matrixID)).Scan(&dcID)
	if err != nil {
		return 0
	}
	return dcID
}

func getMatrixID(dcID uint32) id.EventID {
	var matrixID string
	err := mappingDB.QueryRow("SELECT matrix_id FROM msg_mapping WHERE dc_id = ?", dcID).Scan(&matrixID)
	if err != nil {
		return ""
	}
	return id.EventID(matrixID)
}

// Thread-safe deduplication cache
var (
	lastMessages = make(map[string]time.Time)
	cacheMutex   sync.Mutex
)

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

func stripMatrixUser(mxid string) string {
	if len(mxid) == 0 {
		return mxid
	}
	user := strings.TrimPrefix(mxid, "@")
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
				log.Println("Bridge: Too many requests. Waiting 60s...")
				time.Sleep(60 * time.Second)
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
	dBot, err := NewDeltaChatBot(dbPath, dcAdmin, dcBotName)
	if err != nil {
		log.Fatalf("Failed to initialize Delta Chat bot: %v", err)
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
			
			cleanUser := stripMatrixUser(msg.Sender)
			formatted := fmt.Sprintf("[matrix] %s: %s", cleanUser, msg.Body)

			if msg.IsEdit {
				dcID := getDCID(msg.EditEventID)
				if dcID != 0 {
					dBot.EditMessage(dcID, formatted+" (bearbeitet)")
				}
				continue
			}

			var dcID uint32
			if msg.File != nil {
				dcID = dBot.SendMedia(formatted, msg.File.Name())
				msg.File.Close()
				os.Remove(msg.File.Name())
			} else {
				dcID = dBot.SendMessage(formatted)
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
				dBot.React(dcID, react.Emoji)
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
			if msg.File != nil {
				mBot.SendMessage(fmt.Sprintf("[deltachat] %s:", msg.SenderName))
				mType := event.MsgImage
				ext := strings.ToLower(filepath.Ext(msg.File.Name()))
				switch ext {
				case ".mp4", ".mov", ".avi": mType = event.MsgVideo
				case ".mp3", ".ogg", ".wav": mType = event.MsgAudio
				default: mType = event.MsgImage
				}
				mEventID = mBot.SendMedia(msg.Body, msg.File.Name(), mType)
				msg.File.Close()
				os.Remove(msg.File.Name())
			} else {
				formatted := fmt.Sprintf("[deltachat] %s: %s", msg.SenderName, msg.Body)
				mEventID = mBot.SendMessage(formatted)
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

	// Start Bots
	go func() {
		err := mBot.Start(matrixToDcChan, matrixReactChan, func(roomId id.RoomID) {
			cfg.MatrixRoom = roomId
			saveConfig(cfg)
			log.Printf("Bridge: Matrix room updated: %s", roomId)
		})
		if err != nil {
			log.Fatalf("Matrix bot crashed: %v", err)
		}
	}()

	go dBot.Start(dcToMatrixChan, dcReactChan, func(chatId uint32) {
		cfg.DCChatID = chatId
		saveConfig(cfg)
		log.Printf("Bridge: Delta Chat ID updated: %d", chatId)
	})

	log.Println("Bridge: Running.")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Bridge: Shutting down...")
}
