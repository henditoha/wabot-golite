package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	waSuffix        = "@s.whatsapp.net"
	statusPending   = "PENDING"
	statusTerkirim  = "TERKIRIM"
	statusFailed    = "FAILED"
	senderName      = "Sistem Informasi"
	uiSettingsRoute = "/ui/pengaturan"
)

var syncInterval time.Duration
var isProcessing = false
var processMu sync.Mutex

type APIEndpoint struct {
	ID             int
	NamaSistem     string
	URL            string
	AuthType       string
	Username       string
	Password       string
	Token          string
	CronExpression string
	LastSyncTime   *time.Time
	IsActive       bool
}

type ExternalMessageRequest struct {
	Phone   string `json:"phone"`
	Message string `json:"message"`
}

// -----------------------------------------------------------------------------
// UNIVERSAL PAYLOAD DENGAN KOMPATIBILITAS MUNDUR (BACKWARD COMPATIBILITY)
// -----------------------------------------------------------------------------
type UniversalPayload struct {
	ID        interface{}       `json:"id"`
	NoHP      string            `json:"no_hp"`
	Nama      string            `json:"nama"`
	KodeAcara string            `json:"kode_acara"`
	Status    string            `json:"status"`
	IsiPesan  string            `json:"isi_pesan"`
	Variabel  map[string]string `json:"variabel"`

	// --- LEGACY FIELDS (Dukungan untuk API PHP HPII Banten yang lama) ---
	NamaAcara    string `json:"nama_acara"`
	TanggalAcara string `json:"tanggal_acara"`
	JamMulai     string `json:"jam_mulai"`
	JamSelesai   string `json:"jam_selesai"`
}

type pendingJob struct {
	IDServer   int
	Nama       string
	NoHp       string
	Pesan      string
	RetryCount int
}

func formatPhoneNumber(phone string) string {
	phone = strings.TrimSpace(phone)
	if strings.HasPrefix(phone, "0") {
		return "62" + phone[1:]
	}
	return phone
}

func StartListener() {
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ File .env tidak ditemukan, menggunakan variabel environment sistem")
	}

	syncInterval = 2 * time.Second

	initDatabaseTables()

	go func() {
		log.Printf("⏰ Heartbeat Scheduler Engine Aktif (Denyut: %v)", syncInterval)
		ticker := time.NewTicker(syncInterval)
		for range ticker.C {
			triggerAutoBroadcast()
		}
	}()

	app := fiber.New(fiber.Config{
		AppName:   "WhatsApp External Listener API",
		BodyLimit: 50 * 1024 * 1024,
	})
	app.Use(logger.New())

	api := app.Group("/api/v1")
	api.Post("/send", handleExternalSend)
	api.Get("/broadcast-peserta", handleFetchAndBroadcast)

	app.Get(uiSettingsRoute, handleGetUIPengaturan)
	app.Post(uiSettingsRoute, handlePostUIPengaturan)

	port := os.Getenv("LISTENER_PORT")
	if port == "" {
		port = "5008"
	}

	fmt.Printf("\n📡 HTTP Listener API Aktif di: http://localhost:%s\n", port)
	fmt.Printf("🖥️  Buka UI Pengaturan Jam di: http://localhost:%s%s\n\n", port, uiSettingsRoute)

	if errListen := app.Listen(":" + port); errListen != nil {
		log.Fatalf("❌ Gagal menjalankan HTTP Listener: %v", errListen)
	}
}

func initDatabaseTables() {
	_, errDbApi := dbConn.Exec(`CREATE TABLE IF NOT EXISTS api_endpoints (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		nama_sistem TEXT,
		url TEXT,
		auth_type TEXT DEFAULT 'BASIC',
		username TEXT,
		password TEXT,
		token TEXT,
		cron_expression TEXT DEFAULT '0 */10 * * * *',
		last_sync_time TEXT,
		is_active BOOLEAN DEFAULT 1
	);`)
	if errDbApi != nil {
		log.Printf("⚠️ Gagal membuat tabel api_endpoints: %v", errDbApi)
	}

	_, _ = dbConn.Exec(`ALTER TABLE api_endpoints ADD COLUMN auth_type TEXT DEFAULT 'BASIC';`)
	_, _ = dbConn.Exec(`ALTER TABLE api_endpoints ADD COLUMN token TEXT;`)
	_, _ = dbConn.Exec(`ALTER TABLE api_endpoints ADD COLUMN cron_expression TEXT DEFAULT '0 */10 * * * *';`)
	_, _ = dbConn.Exec(`ALTER TABLE api_endpoints ADD COLUMN last_sync_time TEXT;`)

	var count int
	dbConn.QueryRow("SELECT COUNT(*) FROM api_endpoints").Scan(&count)
	if count == 0 {
		oldURL := os.Getenv("API_URL")
		oldUser := os.Getenv("API_USER")
		oldPass := os.Getenv("API_PASS")
		if oldURL != "" {
			_, errMigrate := dbConn.Exec("INSERT INTO api_endpoints (nama_sistem, url, auth_type, username, password, cron_expression, is_active) VALUES (?, ?, 'BASIC', ?, ?, '0 */10 * * * *', 1)", "HPII Banten", oldURL, oldUser, oldPass)
			if errMigrate == nil {
				log.Println("✅ Berhasil memigrasi kredensial HPII Banten ke sistem penjadwalan Cron.")
			}
		}
	}

	_, errDb1 := dbConn.Exec(`CREATE TABLE IF NOT EXISTS autoresponse (
        id_server INTEGER PRIMARY KEY AUTOINCREMENT,
        ref_id TEXT,
        sistem_asal TEXT,
        nama TEXT,
        no_hp TEXT,
        pesan TEXT,
        status_kirim TEXT,
        jam_kirim DATETIME,
        waktu_sinkron DATETIME DEFAULT CURRENT_TIMESTAMP,
        retry_count INTEGER DEFAULT 0
    );`)
	if errDb1 != nil {
		log.Printf("⚠️ Gagal membuat tabel autoresponse: %v", errDb1)
	}

	_, _ = dbConn.Exec(`ALTER TABLE autoresponse ADD COLUMN retry_count INTEGER DEFAULT 0;`)
	_, _ = dbConn.Exec(`ALTER TABLE autoresponse ADD COLUMN ref_id TEXT;`)
	_, _ = dbConn.Exec(`ALTER TABLE autoresponse ADD COLUMN sistem_asal TEXT;`)

	_, errDbTpl := dbConn.Exec(`CREATE TABLE IF NOT EXISTS tb_template_pesan (
        kode_acara TEXT,
        status TEXT,
        isi_template TEXT NOT NULL,
        PRIMARY KEY (kode_acara, status)
    );`)
	if errDbTpl != nil {
		log.Printf("⚠️ Gagal membuat tabel tb_template_pesan: %v", errDbTpl)
	}

	_, errSet := dbConn.Exec(`CREATE TABLE IF NOT EXISTS pengaturan_sistem (
        id INTEGER PRIMARY KEY,
        jam_buka TEXT,
        jam_tutup TEXT
    );`)
	if errSet != nil {
		log.Printf("⚠️ Gagal membuat tabel pengaturan_sistem: %v", errSet)
	}

	_, _ = dbConn.Exec(`INSERT OR IGNORE INTO pengaturan_sistem (id, jam_buka, jam_tutup) VALUES (1, '05:00', '20:00');`)
}

// -----------------------------------------------------------------------------
// WEB UI HANDLER
// -----------------------------------------------------------------------------

func handleGetUIPengaturan(c *fiber.Ctx) error {
	buka, tutup := getJamOperasional()

	html := fmt.Sprintf(`
    <!DOCTYPE html>
    <html lang="id">
    <head>
        <meta charset="UTF-8">
        <meta name="viewport" content="width=device-width, initial-scale=1.0">
        <title>Pengaturan Bot WA</title>
        <script src="https://cdn.tailwindcss.com"></script>
    </head>
    <body class="bg-gray-100 flex items-center justify-center h-screen">
        <div class="bg-white p-8 rounded-lg shadow-md w-96">
            <h2 class="text-2xl font-bold mb-6 text-gray-800 border-b pb-2">Jendela Pengiriman WA</h2>
            <form action="%s" method="POST">
                <div class="mb-4">
                    <label class="block text-gray-700 font-semibold mb-2">Jam Buka (HH:MM)</label>
                    <input type="time" name="jam_buka" value="%s" class="w-full border-gray-300 border p-2 rounded focus:outline-none focus:ring-2 focus:ring-blue-500" required>
                </div>
                <div class="mb-6">
                    <label class="block text-gray-700 font-semibold mb-2">Jam Tutup (HH:MM)</label>
                    <input type="time" name="jam_tutup" value="%s" class="w-full border-gray-300 border p-2 rounded focus:outline-none focus:ring-2 focus:ring-blue-500" required>
                </div>
                <button type="submit" class="w-full bg-blue-600 hover:bg-blue-700 text-white font-bold py-2 px-4 rounded transition duration-200">
                    Simpan Pengaturan
                </button>
            </form>
        </div>
    </body>
    </html>
    `, uiSettingsRoute, buka, tutup)

	c.Set("Content-Type", "text/html")
	return c.SendString(html)
}

func handlePostUIPengaturan(c *fiber.Ctx) error {
	jamBuka := c.FormValue("jam_buka")
	jamTutup := c.FormValue("jam_tutup")

	if len(jamBuka) >= 4 && len(jamTutup) >= 4 {
		_, errUpdate := dbConn.Exec("UPDATE pengaturan_sistem SET jam_buka = ?, jam_tutup = ? WHERE id = 1", jamBuka, jamTutup)
		if errUpdate != nil {
			log.Printf("❌ Gagal menyimpan pengaturan UI: %v", errUpdate)
		} else {
			log.Printf("✅ Pengaturan Jam Operasional diperbarui: %s sd %s", jamBuka, jamTutup)
		}
	}
	return c.Redirect(uiSettingsRoute)
}

// -----------------------------------------------------------------------------
// LOGIKA WAKTU DINAMIS
// -----------------------------------------------------------------------------

func getJamOperasional() (string, string) {
	var buka, tutup string
	errQuery := dbConn.QueryRow("SELECT jam_buka, jam_tutup FROM pengaturan_sistem WHERE id = 1").Scan(&buka, &tutup)
	if errQuery != nil {
		return "05:00", "20:00"
	}
	return buka, tutup
}

func isSendingWindow(now time.Time) bool {
	buka, tutup := getJamOperasional()
	nowStr := now.Format("15:04")
	return nowStr >= buka && nowStr < tutup
}

func calculateNextSendTime(now time.Time) time.Time {
	buka, tutup := getJamOperasional()
	bukaParts := strings.Split(buka, ":")
	jamBuka, _ := strconv.Atoi(bukaParts[0])
	menitBuka, _ := strconv.Atoi(bukaParts[1])

	nowStr := now.Format("15:04")

	if nowStr < buka {
		return time.Date(now.Year(), now.Month(), now.Day(), jamBuka, menitBuka, 0, 0, now.Location())
	}
	if nowStr >= tutup {
		nextDay := now.AddDate(0, 0, 1)
		return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), jamBuka, menitBuka, 0, 0, now.Location())
	}
	return now
}

func getTemplateFromDB(kodeAcara string, status string) string {
	var isi string
	statusUpper := strings.ToUpper(status)

	errQuery := dbConn.QueryRow("SELECT isi_template FROM tb_template_pesan WHERE kode_acara = ? AND status = ?", kodeAcara, statusUpper).Scan(&isi)
	if errQuery != nil {
		if statusUpper == "BELUM" {
			return "Yth Bapak/Ibu {{NAMA}},\n\nKami menginformasikan bahwa data pendaftaran untuk:\n🖥️ \"{{ACARA}}\"\n Telah kami catat.\n\nProses verifikasi max 1 x 24 jam. Mohon kesediaannya menunggu.\n\nTerima kasih.\n\n\n *_Pesan dikirim oleh sistem._*"
		}
		return "Yth Bapak/Ibu {{NAMA}},\n\nPembayaran/Pendaftaran Anda untuk:\n🖥️ \"{{ACARA}}\" telah kami terima.\n\n🗓️ Jadwal: {{TANGGAL}}\n⏰ Waktu: {{JAM}}\n\nTerima kasih.\n\n\n*_Pesan dikirim oleh sistem._*"
	}
	return isi
}

func renderMessage(p UniversalPayload) string {
	// Mode 1: Jika sistem luar (Bypass) mengirim teks matang
	if p.IsiPesan != "" {
		return strings.ReplaceAll(p.IsiPesan, "{{NAMA}}", p.Nama)
	}

	// Mode 2: Baca Template Lokal
	template := getTemplateFromDB(p.KodeAcara, p.Status)
	msg := strings.ReplaceAll(template, "{{NAMA}}", p.Nama)

	// -- A. Ganti dari JSON "variabel" (Format API Baru / Python) --
	for key, val := range p.Variabel {
		placeholder := "{{" + key + "}}"
		msg = strings.ReplaceAll(msg, placeholder, val)
	}

	// -- B. Ganti dari Legacy PHP (Format API Lama HPII) --
	if p.NamaAcara != "" {
		msg = strings.ReplaceAll(msg, "{{ACARA}}", p.NamaAcara)
	}
	if p.TanggalAcara != "" {
		msg = strings.ReplaceAll(msg, "{{TANGGAL}}", p.TanggalAcara)
	}
	if p.JamMulai != "" {
		jamText := p.JamMulai
		// Format "08:00 sd 12:00 WIB"
		if p.JamSelesai != "" && p.JamSelesai != "-" {
			jamText += " sd " + p.JamSelesai + " WIB"
		} else {
			jamText += " WIB"
		}
		msg = strings.ReplaceAll(msg, "{{JAM}}", jamText)
	}

	return msg
}

// -----------------------------------------------------------------------------
// LOGIKA INTI: CRON PARSER MULTI-ENDPOINT & BROADCAST
// -----------------------------------------------------------------------------

func setAPIAuthorization(req *http.Request, ep APIEndpoint) {
	authType := strings.ToUpper(ep.AuthType)

	if authType == "BEARER" && ep.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Token)
	} else if authType == "BASIC" && ep.Username != "" {
		req.SetBasicAuth(ep.Username, ep.Password)
	} else if authType == "HEADER" && ep.Token != "" {
		req.Header.Set("x-api-key", ep.Token)
	}
}

func isWhatsAppReady() bool {
	return waClient != nil && waClient.IsConnected() && waClient.Store != nil && waClient.Store.ID != nil
}

func acquireProcessLock() bool {
	processMu.Lock()
	if isProcessing {
		processMu.Unlock()
		return false
	}
	isProcessing = true
	processMu.Unlock()
	return true
}

func releaseProcessLock() {
	processMu.Lock()
	isProcessing = false
	processMu.Unlock()
}

func getActiveEndpoints() ([]APIEndpoint, error) {
	rows, err := dbConn.Query("SELECT id, nama_sistem, url, COALESCE(auth_type, 'BASIC'), COALESCE(username, ''), COALESCE(password, ''), COALESCE(token, ''), COALESCE(cron_expression, '0 */10 * * * *'), last_sync_time, is_active FROM api_endpoints WHERE is_active = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var endpoints []APIEndpoint
	for rows.Next() {
		var ep APIEndpoint
		var lastSyncStr sql.NullString

		if err := rows.Scan(&ep.ID, &ep.NamaSistem, &ep.URL, &ep.AuthType, &ep.Username, &ep.Password, &ep.Token, &ep.CronExpression, &lastSyncStr, &ep.IsActive); err == nil {
			if lastSyncStr.Valid && lastSyncStr.String != "" {
				t, errParse := time.ParseInLocation("2006-01-02 15:04:05", lastSyncStr.String, time.Local)
				if errParse == nil {
					ep.LastSyncTime = &t
				}
			}
			endpoints = append(endpoints, ep)
		}
	}
	return endpoints, nil
}

func isEndpointDue(ep APIEndpoint, sched cron.Schedule, now time.Time) bool {
	if ep.LastSyncTime == nil {
		return true
	}
	nextExecutionTime := sched.Next(*ep.LastSyncTime)
	return now.After(nextExecutionTime) || now.Equal(nextExecutionTime)
}

func processEndpointsSchedule(endpoints []APIEndpoint) {
	now := time.Now()
	cronParser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	for _, ep := range endpoints {
		if ep.URL == "" {
			continue
		}

		sched, errParseCron := cronParser.Parse(ep.CronExpression)
		if errParseCron != nil {
			log.Printf("⚠️ Format Cron '%s' salah pada sistem %s. Dilewati.", ep.CronExpression, ep.NamaSistem)
			continue
		}

		if isEndpointDue(ep, sched, now) {
			log.Printf("⏱️ Waktu Cron jatuh tempo. Menyiapkan penarikan data untuk: %s", ep.NamaSistem)
			_, _ = dbConn.Exec("UPDATE api_endpoints SET last_sync_time = ? WHERE id = ?", now.Format("2006-01-02 15:04:05"), ep.ID)
			fetchAndProcess(ep)
		}
	}
}

func triggerAutoBroadcast() {
	if !isWhatsAppReady() {
		return
	}

	if !acquireProcessLock() {
		return
	}
	defer releaseProcessLock()

	processPendingQueue()

	endpoints, err := getActiveEndpoints()
	if err != nil {
		log.Printf("❌ Gagal membaca endpoint API dari database: %v", err)
		return
	}

	processEndpointsSchedule(endpoints)
}

func processPendingQueue() {
	now := time.Now()
	if !isSendingWindow(now) {
		return
	}

	rows, errQuery := dbConn.Query("SELECT id_server, nama, no_hp, pesan, COALESCE(retry_count, 0) FROM autoresponse WHERE status_kirim = ? AND jam_kirim <= ?", statusPending, now)
	if errQuery != nil {
		log.Printf("❌ Gagal membaca antrean pesan pending: %v", errQuery)
		return
	}

	var jobs []pendingJob
	for rows.Next() {
		var job pendingJob
		if errScan := rows.Scan(&job.IDServer, &job.Nama, &job.NoHp, &job.Pesan, &job.RetryCount); errScan == nil {
			jobs = append(jobs, job)
		}
	}
	rows.Close()

	for _, job := range jobs {
		processSinglePendingJob(job)
	}
}

func processSinglePendingJob(job pendingJob) {
	targetJID := job.NoHp
	if !strings.Contains(targetJID, "@") {
		targetJID += waSuffix
	}

	target, errJid := types.ParseJID(targetJID)
	if errJid != nil {
		log.Printf("❌ Format nomor tidak valid untuk pending %s: %v", job.NoHp, errJid)
		return
	}

	log.Printf("🚀 Memproses antrean %s untuk: %s (%s)", statusPending, job.Nama, job.NoHp)
	waCtx, waCancel := context.WithTimeout(context.Background(), 20*time.Second)
	resp, errWa := waClient.SendMessage(waCtx, target, &waProto.Message{Conversation: proto.String(job.Pesan)})
	waCancel()

	if errWa != nil {
		handleFailedPendingJob(job, errWa)
		return
	}

	_, errDbUpdate := dbConn.Exec("UPDATE autoresponse SET status_kirim = ?, jam_kirim = ?, retry_count = ? WHERE id_server = ?", statusTerkirim, time.Now(), job.RetryCount, job.IDServer)
	if errDbUpdate != nil {
		log.Printf("⚠️ Gagal update status DB lokal: %v", errDbUpdate)
	}

	saveAndBroadcast(targetJID, senderName, job.Pesan, true, "sent", time.Now(), resp.ID)
	time.Sleep(10 * time.Second)
}

func handleFailedPendingJob(job pendingJob, sendErr error) {
	job.RetryCount++
	log.Printf("❌ Gagal mengirim WA pending ke %s (Percobaan %d/3): %v", job.NoHp, job.RetryCount, sendErr)

	if job.RetryCount >= 3 {
		_, _ = dbConn.Exec("UPDATE autoresponse SET status_kirim = ?, retry_count = ? WHERE id_server = ?", statusFailed, job.RetryCount, job.IDServer)
		log.Printf("⛔ Pesan ke %s dihentikan permanen setelah 3 kali gagal.", job.NoHp)
	} else {
		_, _ = dbConn.Exec("UPDATE autoresponse SET retry_count = ? WHERE id_server = ?", job.RetryCount, job.IDServer)
	}
}

func fetchAndProcess(ep APIEndpoint) {
	log.Printf("🔍 [%s] Mengetuk pintu server API...", ep.NamaSistem)

	pesertaList, errFetch := fetchPesertaFromAPI(ep)
	if errFetch != nil {
		log.Printf("❌ Gagal Fetch API [%s]: %v", ep.NamaSistem, errFetch)
		return
	}

	if len(pesertaList) == 0 {
		log.Printf("📭 [%s] Ping berhasil, namun tidak ada peserta/pendaftar baru.", ep.NamaSistem)
		return
	}

	log.Printf("⏳ [%s] Ditemukan %d data baru. Memulai injeksi ke antrean...", ep.NamaSistem, len(pesertaList))

	for _, peserta := range pesertaList {
		processSinglePeserta(peserta, ep)
	}
	log.Printf("🎉 [%s] Penarikan data selesai.", ep.NamaSistem)
}

func fetchPesertaFromAPI(ep APIEndpoint) ([]UniversalPayload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("gagal buat request HTTP: %v", errReq)
	}

	setAPIAuthorization(req, ep)

	client := &http.Client{}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("gagal hubungi server: %v", errDo)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server merespons dengan HTTP Status: %d", resp.StatusCode)
	}

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("gagal membaca body respons: %v", errRead)
	}

	var pesertaList []UniversalPayload
	if errJson := json.Unmarshal(body, &pesertaList); errJson != nil {
		return nil, fmt.Errorf("JSON Error: %v", errJson)
	}

	return pesertaList, nil
}

func processSinglePeserta(peserta UniversalPayload, ep APIEndpoint) {
	cleanPhone := formatPhoneNumber(peserta.NoHP)
	targetJID := cleanPhone
	if !strings.Contains(targetJID, "@") {
		targetJID += waSuffix
	}
	target, errJid := types.ParseJID(targetJID)
	if errJid != nil {
		log.Printf("❌ Nomor tidak valid (%s) untuk %s dari %s", cleanPhone, peserta.Nama, ep.NamaSistem)
		return
	}

	pesan := renderMessage(peserta)

	if markAsSyncedPHP(peserta, ep) {
		executeNewWaMessage(peserta, cleanPhone, targetJID, target, pesan, ep.NamaSistem)
	} else {
		log.Printf("⚠️ Gagal menandai is_sync di server [%s] (Ref_ID: %v). Batal kirim WA.", ep.NamaSistem, peserta.ID)
	}
}

func executeNewWaMessage(peserta UniversalPayload, cleanPhone, targetJID string, target types.JID, pesan string, namaSistem string) {
	now := time.Now()
	var statusKirim string
	var jamKirim time.Time

	if isSendingWindow(now) {
		statusKirim = statusTerkirim
		jamKirim = now

		log.Printf("Mengeksekusi pengiriman WA [%s] ke: %s (%s) dari %s", strings.ToUpper(peserta.Status), peserta.Nama, cleanPhone, namaSistem)
		waCtx, waCancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, errWa := waClient.SendMessage(waCtx, target, &waProto.Message{Conversation: proto.String(pesan)})
		waCancel()

		if errWa != nil {
			log.Printf("❌ Gagal mengirim WA ke %s: %v", cleanPhone, errWa)
			statusKirim = statusFailed
		} else {
			saveAndBroadcast(targetJID, namaSistem, pesan, true, "sent", now, resp.ID)
		}
		time.Sleep(10 * time.Second)

	} else {
		statusKirim = statusPending
		jamKirim = calculateNextSendTime(now)
		log.Printf("🌙 Di luar jam operasional. Pesan dari [%s] masuk antrean (Jadwal: %v)", namaSistem, jamKirim.Format("15:04"))
	}

	refID := fmt.Sprintf("%v", peserta.ID)
	_, _ = dbConn.Exec("INSERT INTO autoresponse (ref_id, sistem_asal, nama, no_hp, pesan, status_kirim, jam_kirim, waktu_sinkron, retry_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)",
		refID, namaSistem, peserta.Nama, cleanPhone, pesan, statusKirim, jamKirim, now)
}

func markAsSyncedPHP(peserta UniversalPayload, ep APIEndpoint) bool {
	payload, _ := json.Marshal(map[string]interface{}{
		"id":     peserta.ID,
		"status": strings.ToUpper(peserta.Status),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewBuffer(payload))
	if errReq != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	setAPIAuthorization(req, ep)

	client := &http.Client{}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// -----------------------------------------------------------------------------
// EKSTERNAL API MANUAL
// -----------------------------------------------------------------------------

func handleExternalSend(c *fiber.Ctx) error {
	var req ExternalMessageRequest
	if errBody := c.BodyParser(&req); errBody != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "JSON tidak valid"})
	}

	req.Phone = formatPhoneNumber(req.Phone)
	req.Message = strings.TrimSpace(req.Message)

	if req.Phone == "" || req.Message == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "Phone/Message wajib diisi"})
	}

	targetJID := req.Phone
	if !strings.Contains(targetJID, "@") {
		targetJID += waSuffix
	}

	target, errJid := types.ParseJID(targetJID)
	if errJid != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "Format JID salah"})
	}

	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "error": "WA belum login"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	resp, errWa := waClient.SendMessage(ctx, target, &waProto.Message{Conversation: proto.String(req.Message)})
	if errWa != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": errWa.Error()})
	}

	saveAndBroadcast(targetJID, "API Eksternal", req.Message, true, "sent", time.Now(), resp.ID)
	return c.JSON(fiber.Map{"success": true, "message_id": resp.ID})
}

func handleFetchAndBroadcast(c *fiber.Ctx) error {
	processMu.Lock()
	if isProcessing {
		processMu.Unlock()
		return c.Status(fiber.StatusLocked).JSON(fiber.Map{"error": "Proses broadcast sedang berjalan"})
	}
	processMu.Unlock()

	go triggerAutoBroadcast()
	return c.JSON(fiber.Map{"success": true, "message": "Pemicu manual sinkronisasi Multi-Endpoint berhasil dikirim."})
}
