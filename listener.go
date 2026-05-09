package main

import (
	"bytes"
	"context"
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
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Konstanta Global (Mencegah duplikasi literal string untuk SonarQube)
const (
	waSuffix        = "@s.whatsapp.net"
	statusPending   = "PENDING"
	statusTerkirim  = "TERKIRIM"
	statusFailed    = "FAILED"
	senderName      = "HPII Banten"
	uiSettingsRoute = "/ui/pengaturan"
)

// Variabel Konfigurasi Global
var (
	hpiiApiUser  string
	hpiiApiPass  string
	hpiiApiURL   string
	syncInterval time.Duration
)

// Global lock untuk mencegah proses ganda
var isProcessing = false
var processMu sync.Mutex

type ExternalMessageRequest struct {
	Phone   string `json:"phone"`
	Message string `json:"message"`
}

// Struct PesertaHPII dimutakhirkan dengan field Status
type PesertaHPII struct {
	ID           int    `json:"id"`
	KodeAcara    string `json:"kode_acara"`
	Status       string `json:"status"` // LUNAS atau BELUM
	NoHP         string `json:"no_hp"`
	Nama         string `json:"nama"`
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

// StartListener menjalankan server HTTP terpisah & Init DB Lokal
func StartListener() {
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ File .env tidak ditemukan, menggunakan variabel environment sistem")
	}

	hpiiApiUser = os.Getenv("API_USER")
	hpiiApiPass = os.Getenv("API_PASS")
	hpiiApiURL = os.Getenv("API_URL")

	intervalStr := os.Getenv("SYNC_INTERVAL")
	if intervalStr == "" {
		intervalStr = "10m"
	}

	parsedInterval, errDur := time.ParseDuration(intervalStr)
	if errDur != nil {
		log.Printf("⚠️ Format SYNC_INTERVAL salah, fallback ke 10 menit. Error: %v", errDur)
		parsedInterval = 10 * time.Minute
	}
	syncInterval = parsedInterval

	initDatabaseTables()

	go func() {
		log.Printf("⏰ Penjadwal Otomatis Aktif (Setiap %v)", syncInterval)
		ticker := time.NewTicker(syncInterval)
		for range ticker.C {
			triggerAutoBroadcast()
		}
	}()

	app := fiber.New(fiber.Config{AppName: "WhatsApp External Listener API"})
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

// initDatabaseTables memisahkan logika inisialisasi agar fungsi utama tidak terlalu panjang
func initDatabaseTables() {
	_, errDb1 := dbConn.Exec(`CREATE TABLE IF NOT EXISTS peserta_sinkron (
        id_server INTEGER PRIMARY KEY,
        nama TEXT,
        no_hp TEXT,
        pesan TEXT,
        status_kirim TEXT,
        jam_kirim DATETIME,
        waktu_sinkron DATETIME DEFAULT CURRENT_TIMESTAMP,
        retry_count INTEGER DEFAULT 0
    );`)
	if errDb1 != nil {
		log.Printf("⚠️ Gagal membuat tabel peserta_sinkron: %v", errDb1)
	}

	_, errAlt := dbConn.Exec(`ALTER TABLE peserta_sinkron ADD COLUMN retry_count INTEGER DEFAULT 0;`)
	if errAlt != nil {
		// Abaikan error jika kolom sudah ada
	}

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

	_, errIns := dbConn.Exec(`INSERT OR IGNORE INTO pengaturan_sistem (id, jam_buka, jam_tutup) VALUES (1, '05:00', '20:00');`)
	if errIns != nil {
		log.Printf("⚠️ Gagal insert data pengaturan awal: %v", errIns)
	}
}

// -----------------------------------------------------------------------------
// WEB UI HANDLER (FIBER)
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
// LOGIKA WAKTU DINAMIS DARI DATABASE
// -----------------------------------------------------------------------------

func getJamOperasional() (string, string) {
	var buka, tutup string
	errQuery := dbConn.QueryRow("SELECT jam_buka, jam_tutup FROM pengaturan_sistem WHERE id = 1").Scan(&buka, &tutup)
	if errQuery != nil {
		return "05:00", "20:00" // Nilai aman fallback
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

// -----------------------------------------------------------------------------
// LOGIKA RENDER TEMPLATE DARI SQLITE LOKAL
// -----------------------------------------------------------------------------

func getTemplateFromDB(kodeAcara string, status string) string {
	var isi string
	statusUpper := strings.ToUpper(status)

	errQuery := dbConn.QueryRow("SELECT isi_template FROM tb_template_pesan WHERE kode_acara = ? AND status = ?", kodeAcara, statusUpper).Scan(&isi)
	if errQuery != nil {
		if statusUpper == "BELUM" {
			return "Yth Bapak/Ibu {{NAMA}},\n\nKami menginformasikan bahwa pendaftaran Anda untuk acara:\n🖥️ \"{{ACARA}}\" tercatat BELUM LUNAS.\n\nMohon segera menyelesaikan pembayaran agar proses pendaftaran dapat dilanjutkan dan tiket dapat diterbitkan.\n\nTerima kasih,\nTim HPII Banten"
		}
		return "Yth Bapak/Ibu {{NAMA}},\n\nPembayaran Anda untuk acara:\n🖥️ \"{{ACARA}}\" telah kami terima.\n\n🗓️ Jadwal: {{TANGGAL}}\n⏰ Waktu: {{JAM}}\n\nTerima kasih,\nTim HPII Banten"
	}
	return isi
}

func renderMessage(p PesertaHPII) string {
	template := getTemplateFromDB(p.KodeAcara, p.Status)
	jamText := p.JamMulai + " sd " + p.JamSelesai + " WIB"

	msg := strings.ReplaceAll(template, "{{NAMA}}", p.Nama)
	msg = strings.ReplaceAll(msg, "{{ACARA}}", p.NamaAcara)
	msg = strings.ReplaceAll(msg, "{{TANGGAL}}", p.TanggalAcara)
	msg = strings.ReplaceAll(msg, "{{JAM}}", jamText)

	return msg
}

// -----------------------------------------------------------------------------
// PROSES PENGIRIMAN & PENARIKAN DATA
// -----------------------------------------------------------------------------

func triggerAutoBroadcast() {
	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return // Silent return jika WA belum login
	}

	processMu.Lock()
	if isProcessing {
		processMu.Unlock()
		return
	}
	isProcessing = true
	processMu.Unlock()

	defer func() {
		processMu.Lock()
		isProcessing = false
		processMu.Unlock()
	}()

	log.Println("🔄 Memulai siklus sinkronisasi & pengecekan antrean...")
	processPendingQueue()
	fetchAndProcess(hpiiApiURL)
}

func processPendingQueue() {
	now := time.Now()
	if !isSendingWindow(now) {
		return
	}

	rows, errQuery := dbConn.Query("SELECT id_server, nama, no_hp, pesan, COALESCE(retry_count, 0) FROM peserta_sinkron WHERE status_kirim = ? AND jam_kirim <= ?", statusPending, now)
	if errQuery != nil {
		log.Printf("❌ Gagal membaca antrean pesan pending: %v", errQuery)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var job pendingJob
		if errScan := rows.Scan(&job.IDServer, &job.Nama, &job.NoHp, &job.Pesan, &job.RetryCount); errScan != nil {
			continue
		}
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

	_, errDbUpdate := dbConn.Exec("UPDATE peserta_sinkron SET status_kirim = ?, jam_kirim = ?, retry_count = ? WHERE id_server = ?", statusTerkirim, time.Now(), job.RetryCount, job.IDServer)
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
		_, errDbFail := dbConn.Exec("UPDATE peserta_sinkron SET status_kirim = ?, retry_count = ? WHERE id_server = ?", statusFailed, job.RetryCount, job.IDServer)
		if errDbFail != nil {
			log.Printf("⚠️ Gagal update DB lokal ke FAILED: %v", errDbFail)
		}
		log.Printf("⛔ Pesan ke %s dihentikan permanen (Status %s) setelah 3 kali gagal.", job.NoHp, statusFailed)
	} else {
		_, errDbRetry := dbConn.Exec("UPDATE peserta_sinkron SET retry_count = ? WHERE id_server = ?", job.RetryCount, job.IDServer)
		if errDbRetry != nil {
			log.Printf("⚠️ Gagal update retry_count DB lokal: %v", errDbRetry)
		}
	}
}

func fetchAndProcess(apiURL string) {
	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return // Double safety check
	}

	pesertaList, errFetch := fetchPesertaFromAPI(apiURL)
	if errFetch != nil {
		log.Printf("❌ Gagal Fetch API: %v", errFetch)
		return
	}

	if len(pesertaList) == 0 {
		log.Println("📭 Info: Data API kosong (Array []). Tidak ada pesan baru untuk diproses.")
		return
	}

	log.Printf("⏳ Ditemukan %d tiket peserta baru. Mulai sinkronisasi...", len(pesertaList))

	for _, peserta := range pesertaList {
		processSinglePeserta(peserta, apiURL)
	}
	log.Println("🎉 Siklus sinkronisasi API selesai.")
}

func fetchPesertaFromAPI(apiURL string) ([]PesertaHPII, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("gagal buat request HTTP: %v", errReq)
	}

	req.SetBasicAuth(hpiiApiUser, hpiiApiPass)

	client := &http.Client{}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("gagal hubungi server PHP: %v", errDo)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server PHP merespons dengan status error: %d", resp.StatusCode)
	}

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("gagal membaca body respons: %v", errRead)
	}

	var pesertaList []PesertaHPII
	if errJson := json.Unmarshal(body, &pesertaList); errJson != nil {
		return nil, fmt.Errorf("JSON Error: %v", errJson)
	}

	return pesertaList, nil
}

// Direfactor untuk memangkas Cognitive Complexity
func processSinglePeserta(peserta PesertaHPII, apiURL string) {
	cleanPhone := formatPhoneNumber(peserta.NoHP)
	targetJID := cleanPhone
	if !strings.Contains(targetJID, "@") {
		targetJID += waSuffix
	}
	target, errJid := types.ParseJID(targetJID)
	if errJid != nil {
		log.Printf("❌ Nomor tidak valid (%s) untuk %s", cleanPhone, peserta.Nama)
		return
	}

	pesan := renderMessage(peserta)

	// Sekarang bot melempar peserta.Status ke fungsi markAsSyncedPHP
	if markAsSyncedPHP(peserta.ID, peserta.Status, apiURL) {
		executeNewWaMessage(peserta, cleanPhone, targetJID, target, pesan)
	} else {
		log.Printf("⚠️ Gagal update status is_sync di server PHP (ID: %d). Membatalkan eksekusi WA.", peserta.ID)
	}
}

// Logika inti pengiriman dipisah agar lebih modular
func executeNewWaMessage(peserta PesertaHPII, cleanPhone, targetJID string, target types.JID, pesan string) {
	now := time.Now()
	var statusKirim string
	var jamKirim time.Time

	if isSendingWindow(now) {
		statusKirim = statusTerkirim
		jamKirim = now

		log.Printf("Mengeksekusi pengiriman WA [%s] ke: %s (%s)", strings.ToUpper(peserta.Status), peserta.Nama, cleanPhone)
		waCtx, waCancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, errWa := waClient.SendMessage(waCtx, target, &waProto.Message{Conversation: proto.String(pesan)})
		waCancel()

		if errWa != nil {
			log.Printf("❌ Gagal mengirim WA ke %s: %v", cleanPhone, errWa)
			statusKirim = statusFailed
		} else {
			saveAndBroadcast(targetJID, senderName, pesan, true, "sent", now, resp.ID)
		}
		time.Sleep(10 * time.Second)

	} else {
		statusKirim = statusPending
		jamKirim = calculateNextSendTime(now)
		log.Printf("🌙 Di luar jam operasional. Pesan [%s] masuk antrean %s (Jadwal: %v)", strings.ToUpper(peserta.Status), statusPending, jamKirim.Format("15:04"))
	}

	_, errDbIns := dbConn.Exec("INSERT OR REPLACE INTO peserta_sinkron (id_server, nama, no_hp, pesan, status_kirim, jam_kirim, waktu_sinkron, retry_count) VALUES (?, ?, ?, ?, ?, ?, ?, 0)",
		peserta.ID, peserta.Nama, cleanPhone, pesan, statusKirim, jamKirim, now)
	if errDbIns != nil {
		log.Printf("⚠️ WA diproses, tapi gagal menyimpan ke history SQLite (ID: %d): %v", peserta.ID, errDbIns)
	}
}

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
	return c.JSON(fiber.Map{"success": true, "message": "Pemicu manual berhasil."})
}

// markAsSyncedPHP kini mengirim ID beserta Status (misal: "BELUM" / "LUNAS") ke PHP
func markAsSyncedPHP(id int, status string, apiURL string) bool {
	// Menambahkan payload status agar bisa dibaca jika PHP butuh melakukan logic khusus
	payload, _ := json.Marshal(map[string]interface{}{
		"id":     id,
		"status": strings.ToUpper(status),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(payload))
	if errReq != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(hpiiApiUser, hpiiApiPass)

	client := &http.Client{}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
