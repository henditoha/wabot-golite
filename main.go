package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/websocket/v2"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/glebarez/sqlite"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- KONSTANTA & STRUKTUR DATA ---
const (
	routePesan = "/pesan"
	dbDriver   = "sqlite"
)

type Message struct {
	ID          string `json:"id"`
	ChatJid     string `json:"chat_jid"`
	SenderJid   string `json:"sender_jid"`
	SenderName  string `json:"sender_name"`
	Content     string `json:"content"`
	IsFromMe    bool   `json:"is_from_me"`
	Timestamp   string `json:"timestamp"`
	Status      string `json:"status"`
	DisplayTime string `json:"display_time"`
	Type        string `json:"type,omitempty"`
}

var (
	waClient  *whatsmeow.Client
	dbConn    *sql.DB
	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex
	myJID     string

	currentQR string
	qrMu      sync.RWMutex
)

func init() {
	_ = godotenv.Load()
}

func main() {
	initSQLite()
	initWhatsApp()

	app := fiber.New(fiber.Config{
		AppName: "Nursing Informatics System v1.1",
	})

	app.Use(logger.New())
	app.Static("/static", "./templates")

	// Routes Utama
	app.Get("/", func(c *fiber.Ctx) error { return c.Redirect(routePesan) })
	app.Get(routePesan, PesanPageHandler(dbConn))
	app.Get("/login", LoginHandler)

	// API Routes (Internal Dashboard)
	api := app.Group("/api")
	api.Get("/messages/:jid", getMessages)
	api.Post("/send", sendMessage)
	api.Get("/qr", getQR)
	api.Get("/status", getStatus)

	// IMPLEMENTASI TUNTAS: Menerima request Mark as Read
	api.Post("/read/:jid", markAsRead)

	app.Get("/ws", websocket.New(wsHandler))

	// Jalankan WhatsApp Engine & Listener API secara konkuren
	go connectWA()
	go StartListener()

	handleSignals(app)

	port := os.Getenv("APP_PORT")
	if port == "" {
		port = "5007"
	}
	fmt.Printf("\n🚀 Dashboard Aktif: http://localhost:%s\n", port)

	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Gagal menjalankan server: %v", err)
	}
}

// --- HELPER FUNCTIONS ---
func formatWATime(tsStr string) string {
	t, err := time.Parse("2006-01-02 15:04:05", tsStr)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, tsStr)
	}
	now := time.Now()

	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	if t.Year() == now.Year() && t.YearDay() == now.YearDay()-1 {
		return "Kemarin"
	}
	return t.Format("02/01/06")
}

func broadcastData(data []byte) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for client := range clients {
		_ = client.WriteMessage(websocket.TextMessage, data)
	}
}

// --- HANDLERS ---
func LoginHandler(c *fiber.Ctx) error {
	if waClient != nil && waClient.Store.ID != nil {
		return c.Redirect(routePesan)
	}

	tmpl, err := template.ParseFiles("templates/login.html")
	if err != nil {
		return c.Status(500).SendString("<h1>Template Error</h1><p>" + err.Error() + "</p>")
	}

	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, nil)
	return c.Send(buf.Bytes())
}

func PesanPageHandler(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if waClient == nil || waClient.Store.ID == nil {
			return c.Redirect("/login")
		}

		querySidebar := `
            SELECT chat_jid, sender_name, content, MAX(timestamp) as timestamp 
            FROM (
                SELECT chat_jid, sender_name, content, timestamp 
                FROM messages 
                WHERE rowid IN (SELECT MAX(rowid) FROM messages GROUP BY chat_jid)
                
                UNION ALL
                
                SELECT chat_id as chat_jid, sender_name, message as content, created_at as timestamp
                FROM group_chat_logs
                WHERE id IN (SELECT MAX(id) FROM group_chat_logs GROUP BY chat_id)
            )
            GROUP BY chat_jid
            ORDER BY timestamp DESC`

		rows, err := db.Query(querySidebar)
		if err != nil {
			return c.Status(500).SendString("<h1>Database Error</h1><p>" + err.Error() + "</p>")
		}
		defer rows.Close()

		var contacts []fiber.Map
		for rows.Next() {
			var jid, name, content, ts string
			if err := rows.Scan(&jid, &name, &content, &ts); err != nil {
				continue
			}
			contacts = append(contacts, fiber.Map{
				"Jid":         jid,
				"Name":        name,
				"LastMessage": content,
				"Time":        formatWATime(ts),
				"IsGroup":     strings.Contains(jid, "@g.us"),
			})
		}

		tmpl, err := template.ParseFiles("templates/index.html", "templates/pesan.html")
		if err != nil {
			return c.Status(500).SendString("<h1>Template Error</h1><p>" + err.Error() + "</p>")
		}

		var buf bytes.Buffer
		err = tmpl.ExecuteTemplate(&buf, "index.html", fiber.Map{
			"UserTitle": "Ns. Hendi, S.Kep., MMSI",
			"Contacts":  contacts,
		})

		if err != nil {
			return c.Status(500).SendString("<h1>Execution Error</h1><p>" + err.Error() + "</p>")
		}

		c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
		return c.Send(buf.Bytes())
	}
}

func getMessages(c *fiber.Ctx) error {
	jid := c.Params("jid")
	isGroup := strings.Contains(jid, "@g.us")
	msgs := make([]Message, 0)

	var rows *sql.Rows
	var err error

	if isGroup {
		rows, err = dbConn.Query(`
            SELECT CAST(id AS TEXT), chat_id, COALESCE(sender_name, ''), message, 
                   CASE WHEN sender_name = 'Anda' THEN 1 ELSE 0 END as is_from_me, 
                   'read' as status, created_at 
            FROM group_chat_logs 
            WHERE chat_id=? 
            ORDER BY created_at ASC LIMIT 100`, jid)
	} else {
		rows, err = dbConn.Query(`
            SELECT id, chat_jid, sender_name, content, is_from_me, status, timestamp 
            FROM messages 
            WHERE chat_jid=? 
            ORDER BY timestamp ASC LIMIT 100`, jid)
	}

	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	for rows.Next() {
		var m Message
		_ = rows.Scan(&m.ID, &m.ChatJid, &m.SenderName, &m.Content, &m.IsFromMe, &m.Status, &m.Timestamp)
		m.DisplayTime = formatWATime(m.Timestamp)
		msgs = append(msgs, m)
	}

	return c.JSON(msgs)
}

func sendMessage(c *fiber.Ctx) error {
	var req struct {
		JID string `json:"jid"`
		Msg string `json:"message"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).SendString("Invalid JSON")
	}

	target, err := types.ParseJID(req.JID)
	if err != nil {
		return c.Status(400).SendString("Invalid JID")
	}

	msgID := uuid.New().String()
	status := "sent"
	if waClient != nil && waClient.IsConnected() {
		resp, err := waClient.SendMessage(context.Background(), target, &waProto.Message{Conversation: proto.String(req.Msg)})
		if err == nil {
			msgID = resp.ID
		} else {
			status = "error"
		}
	}

	saveAndBroadcast(req.JID, "Anda", req.Msg, true, status, time.Now(), msgID)
	return c.JSON(fiber.Map{"status": "ok", "id": msgID})
}

// -------------------------------------------------------------------------
// REFACTOR SONARQUBE & COMPILER: Logika markAsRead dipecah menjadi fungsi-fungsi kecil
// -------------------------------------------------------------------------

func markAsRead(c *fiber.Ctx) error {
	jid := c.Params("jid")
	target, err := types.ParseJID(jid)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid JID"})
	}

	// Jangan proses centang biru jika ini adalah grup
	if strings.Contains(jid, "@g.us") {
		return c.JSON(fiber.Map{"success": true, "message": "Grup diabaikan untuk status read", "read_count": 0})
	}

	msgIDs, senderJID := fetchUnreadMessages(jid, target)

	if len(msgIDs) > 0 {
		sendReadReceiptToWA(msgIDs, target, senderJID)
		updateLocalMessagesToRead(jid)
	}

	return c.JSON(fiber.Map{
		"success":    true,
		"message":    "Pesan berhasil ditandai dibaca secara nyata",
		"read_count": len(msgIDs),
	})
}

// Helper 1: Menarik ID pesan yang belum dibaca dari database lokal
func fetchUnreadMessages(chatJid string, defaultSender types.JID) ([]types.MessageID, types.JID) {
	var msgIDs []types.MessageID
	senderJID := defaultSender

	rows, err := dbConn.Query("SELECT id, sender_jid FROM messages WHERE chat_jid=? AND is_from_me=0 AND status != 'read'", chatJid)
	if err != nil {
		return msgIDs, senderJID
	}
	defer rows.Close()

	for rows.Next() {
		var mID, sJID string
		if err := rows.Scan(&mID, &sJID); err == nil {
			msgIDs = append(msgIDs, types.MessageID(mID)) // Format compiler yang benar
			if senderJID == defaultSender && sJID != "" {
				if parsed, err := types.ParseJID(sJID); err == nil {
					senderJID = parsed
				}
			}
		}
	}
	return msgIDs, senderJID
}

// Helper 2: Mengirimkan sinyal centang biru nyata ke Server WhatsApp
func sendReadReceiptToWA(msgIDs []types.MessageID, chatJid types.JID, senderJid types.JID) {
	if waClient == nil || !waClient.IsConnected() {
		return
	}

	// FIX COMPILER: Menambahkan context.Background() sesuai signature terbaru whatsmeow
	err := waClient.MarkRead(context.Background(), msgIDs, time.Now(), chatJid, senderJid)
	if err != nil {
		log.Printf("⚠️ Gagal mengirim status read ke server WA: %v", err)
	}
}

// Helper 3: Memperbarui status baca di database lokal
func updateLocalMessagesToRead(chatJid string) {
	_, _ = dbConn.Exec("UPDATE messages SET status='read' WHERE chat_jid=? AND is_from_me=0", chatJid)
}

// -------------------------------------------------------------------------

func getQR(c *fiber.Ctx) error {
	qrMu.RLock()
	defer qrMu.RUnlock()
	return c.JSON(fiber.Map{"qr": currentQR})
}

func getStatus(c *fiber.Ctx) error {
	if waClient == nil {
		return c.JSON(fiber.Map{"logged_in": false, "connected": false})
	}
	return c.JSON(fiber.Map{
		"logged_in": waClient.Store.ID != nil,
		"connected": waClient.IsConnected(),
	})
}

// --- CORE LOGIC: SAVE & BROADCAST ---
func saveAndBroadcast(jid, name, content string, isMe bool, status string, ts time.Time, msgID string) {
	tsStr := ts.Format("2006-01-02 15:04:05")
	senderJid := jid
	isGroup := strings.Contains(jid, "@g.us")
	finalID := msgID

	if isMe {
		senderJid = myJID
	}

	if isGroup {
		res, err := dbConn.Exec(`INSERT INTO group_chat_logs (chat_id, sender_name, message, created_at) VALUES (?, ?, ?, ?)`, jid, name, content, tsStr)
		if err != nil {
			log.Printf("DB Save Group Error: %v", err)
		} else {
			idInt, _ := res.LastInsertId()
			finalID = fmt.Sprintf("%d", idInt)
		}
	} else {
		_, err := dbConn.Exec(`INSERT OR REPLACE INTO messages (id, chat_jid, sender_jid, sender_name, content, is_from_me, timestamp, status) 
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, msgID, jid, senderJid, name, content, isMe, tsStr, status)
		if err != nil {
			log.Printf("DB Save Error: %v", err)
		}
	}

	m := Message{
		ID:          finalID,
		ChatJid:     jid,
		SenderName:  name,
		Content:     content,
		IsFromMe:    isMe,
		Status:      status,
		Timestamp:   tsStr,
		DisplayTime: formatWATime(tsStr),
	}

	data, _ := json.Marshal(m)
	broadcastData(data)
}

// --- DATABASE & WA INIT ---
func initSQLite() {
	var err error
	dbConn, err = sql.Open(dbDriver, "wa_asli.db?_cache=shared&_foreign_keys=1")
	if err != nil {
		log.Fatalf("❌ Gagal buka SQLite: %v", err)
	}

	dbConn.SetMaxOpenConns(1)

	queries := []string{
		`CREATE TABLE IF NOT EXISTS messages (
            id TEXT PRIMARY KEY, chat_jid TEXT, sender_jid TEXT, sender_name TEXT, 
            content TEXT, is_from_me BOOLEAN, timestamp DATETIME, status TEXT
        );`,
		`CREATE TABLE IF NOT EXISTS group_chat_logs (
            id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id TEXT, sender_name TEXT, 
            message TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );`,
		`CREATE INDEX IF NOT EXISTS idx_group_chat ON group_chat_logs(chat_id);`,
	}

	for _, q := range queries {
		if _, err := dbConn.Exec(q); err != nil {
			log.Fatalf("❌ Gagal inisialisasi tabel: %v", err)
		}
	}
	fmt.Println("✅ SQLite Siap")
}

func initWhatsApp() {
	container := sqlstore.NewWithDB(dbConn, dbDriver, waLog.Stdout("DB", "ERROR", true))

	if err := container.Upgrade(context.Background()); err != nil {
		log.Fatalf("❌ Gagal upgrade Whatsmeow store: %v", err)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Printf("⚠️ Gagal baca device: %v", err)
	}

	if device == nil {
		device = container.NewDevice()
	}

	waClient = whatsmeow.NewClient(device, waLog.Stdout("Client", "INFO", true))
	waClient.AddEventHandler(waEventHandler)
}

// --- WA EVENT HANDLERS ---
func waEventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		fmt.Println("🟢 Terhubung ke server WhatsApp")
	case *events.Disconnected:
		fmt.Println("🔴 Terputus dari server WhatsApp")
	case *events.LoggedOut:
		fmt.Println("🚨 Sesi Logged Out! Sesi diputus dari HP.")

		broadcastData([]byte(`{"type":"logout", "content":"[LOGOUT]"}`))
		waClient.Disconnect()

		qrMu.Lock()
		currentQR = ""
		qrMu.Unlock()
		go loginNewSession()

	case *events.Message:
		processIncomingMessage(v)
	case *events.Receipt:
		processReceipt(v)
	}
}

func processIncomingMessage(v *events.Message) {
	content := v.Message.GetConversation()
	if content == "" && v.Message.GetExtendedTextMessage() != nil {
		content = v.Message.GetExtendedTextMessage().GetText()
	}

	if content == "" {
		return
	}

	name := v.Info.PushName
	if v.Info.IsFromMe {
		name = "Anda"
	} else if name == "" {
		name = v.Info.Sender.User
	}

	saveAndBroadcast(v.Info.Chat.String(), name, content, v.Info.IsFromMe, "delivered", v.Info.Timestamp, v.Info.ID)
}

func processReceipt(v *events.Receipt) {
	for _, msgID := range v.MessageIDs {
		status := "sent"
		switch v.Type {
		case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
			status = "read"
		case types.ReceiptTypeDelivered:
			status = "delivered"
		}

		_, _ = dbConn.Exec("UPDATE messages SET status=? WHERE id=?", status, msgID)

		m := Message{ID: msgID, Status: status, Content: "[STATUS_UPDATE]"}
		data, _ := json.Marshal(m)
		broadcastData(data)
	}
}

// --- CONNECTION LOGIC ---
func connectWA() {
	if waClient == nil {
		log.Fatal("❌ waClient belum diinisialisasi!")
	}

	if waClient.Store.ID == nil {
		loginNewSession()
	} else {
		reconnectExistingSession()
	}
}

func loginNewSession() {
	qrChan, _ := waClient.GetQRChannel(context.Background())
	if err := waClient.Connect(); err != nil {
		log.Printf("❌ Gagal connect WA: %v", err)
		return
	}
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			fmt.Println("\n📲 SCAN BARKODE INI DENGAN WHATSAPP ANDA:")
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)

			qrMu.Lock()
			currentQR = evt.Code
			qrMu.Unlock()
		case "success":
			fmt.Println("✅ Login Berhasil!")
			qrMu.Lock()
			currentQR = ""
			qrMu.Unlock()
			myJID = waClient.Store.ID.ToNonAD().String()
		}
	}
}

func reconnectExistingSession() {
	fmt.Println("🔄 Mencoba menghubungkan kembali sesi lama...")
	myJID = waClient.Store.ID.ToNonAD().String()

	for {
		if err := waClient.Connect(); err != nil {
			log.Printf("⚠️ Gagal menyambung ke WA: %v. Akan mencoba lagi dalam 5 detik...", err)
			time.Sleep(5 * time.Second)
		} else {
			fmt.Println("✅ WhatsApp Siap Digunakan!")
			break
		}
	}
}

// --- WEBSOCKET & SIGNAL HANDLING ---
func wsHandler(c *websocket.Conn) {
	clientsMu.Lock()
	clients[c] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
		_ = c.Close()
	}()

	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
}

func handleSignals(app *fiber.App) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\n🛑 Menutup aplikasi dengan aman...")
		if waClient != nil {
			waClient.Disconnect()
		}
		if dbConn != nil {
			_ = dbConn.Close()
		}
		_ = app.Shutdown()
		os.Exit(0)
	}()
}
