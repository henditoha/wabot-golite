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
	waSuffix       = "@s.whatsapp.net"
	statusPending  = "PENDING"
	statusTerkirim = "TERKIRIM"
	statusFailed   = "FAILED"
)

// Variabel Konfigurasi Global (Akan diisi dari .env)
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

// Struct PesertaHPII WAJIB memuat KodeAcara dari JSON API PHP
type PesertaHPII struct {
	ID           int    `json:"id"`
	KodeAcara    string `json:"kode_acara"`
	NoHP         string `json:"no_hp"`
	Nama         string `json:"nama"`
	NamaAcara    string `json:"nama_acara"`
	TanggalAcara string `json:"tanggal_acara"`
	JamMulai     string `json:"jam_mulai"`
	JamSelesai   string `json:"jam_selesai"`
}

// Struct pembantu untuk membawa data satu baris antrean agar rapi
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

// StartListener menjalankan server HTTP terpisah (API Gateway) & Init DB Lokal
func StartListener() {
	// 1. Load file .env
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ File .env tidak ditemukan, menggunakan variabel environment sistem")
	}

	// 2. Set Nilai Variabel dari .env
	hpiiApiUser = os.Getenv("API_USER")
	hpiiApiPass = os.Getenv("API_PASS")
	hpiiApiURL = os.Getenv("API_URL")

	intervalStr := os.Getenv("SYNC_INTERVAL")
	if intervalStr == "" {
		intervalStr = "10m" // Fallback jika tidak diset
	}

	parsedInterval, err := time.ParseDuration(intervalStr)
	if err != nil {
		log.Printf("⚠️ Format SYNC_INTERVAL di .env salah, fallback ke 10 menit. Error: %v", err)
		parsedInterval = 10 * time.Minute
	}
	syncInterval = parsedInterval

	// 3. Buat tabel otomatis sesuai spesifikasi
	_, errDb := dbConn.Exec(`CREATE TABLE IF NOT EXISTS peserta_sinkron (
        id_server INTEGER PRIMARY KEY,
        nama TEXT,
        no_hp TEXT,
        pesan TEXT,
        status_kirim TEXT,
        jam_kirim DATETIME,
        waktu_sinkron DATETIME DEFAULT CURRENT_TIMESTAMP,
        retry_count INTEGER DEFAULT 0
    );`)
	if errDb != nil {
		log.Printf("⚠️ Gagal membuat tabel peserta_sinkron: %v", errDb)
	}

	// Migrasi otomatis: Tambahkan kolom retry_count jika menggunakan database versi lama
	_, _ = dbConn.Exec(`ALTER TABLE peserta_sinkron ADD COLUMN retry_count INTEGER DEFAULT 0;`)

	// 4. Buat tabel template pesan lokal sesuai spesifikasi
	_, errDbTpl := dbConn.Exec(`CREATE TABLE IF NOT EXISTS tb_template_pesan (
        kode_acara TEXT PRIMARY KEY,
        isi_template TEXT NOT NULL
    );`)
	if errDbTpl != nil {
		log.Printf("⚠️ Gagal membuat tabel tb_template_pesan: %v", errDbTpl)
	}

	// 5. Jalankan Scheduler
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

	port := os.Getenv("LISTENER_PORT")
	if port == "" {
		port = "5008"
	}

	fmt.Printf("\n📡 HTTP Listener API Aktif di: http://localhost:%s\n", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("❌ Gagal menjalankan HTTP Listener: %v", err)
	}
}

func triggerAutoBroadcast() {
	// Pengecekan senyap: Jika WA belum siap/login, batalkan eksekusi tanpa mencetak log
	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return
	}

	processMu.Lock()
	if isProcessing {
		log.Println("⏳ Proses sinkronisasi sebelumnya masih berjalan...")
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
	// Cek & Kirim pesan tertunda (Pending) dari database lokal
	processPendingQueue()
	// Tarik data baru dari server API PHP
	fetchAndProcess(hpiiApiURL)
}

func handleExternalSend(c *fiber.Ctx) error {
	var req ExternalMessageRequest
	if err := c.BodyParser(&req); err != nil {
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

	target, err := types.ParseJID(targetJID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "Format JID salah"})
	}

	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "error": "WA belum login"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	resp, err := waClient.SendMessage(ctx, target, &waProto.Message{Conversation: proto.String(req.Message)})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": err.Error()})
	}

	saveAndBroadcast(targetJID, "Sistem Eksternal API", req.Message, true, "sent", time.Now(), resp.ID)
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

// -----------------------------------------------------------------------------
// LOGIKA WAKTU (WINDOW PENGIRIMAN 05:00 SD 20:00)
// -----------------------------------------------------------------------------

func isSendingWindow(now time.Time) bool {
	hour := now.Hour()
	return hour >= 5 && hour < 20 // 05:00 hingga 19:59:59
}

func calculateNextSendTime(now time.Time) time.Time {
	if now.Hour() < 5 {
		// Jika ditarik jam 00:00 - 04:59, jadwalkan jam 05:00 hari ini
		return time.Date(now.Year(), now.Month(), now.Day(), 5, 0, 0, 0, now.Location())
	}
	// Jika ditarik jam 20:00 ke atas, jadwalkan jam 05:00 besok
	nextDay := now.AddDate(0, 0, 1)
	return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), 5, 0, 0, 0, now.Location())
}

// -----------------------------------------------------------------------------
// LOGIKA RENDER TEMPLATE DARI SQLITE LOKAL
// -----------------------------------------------------------------------------

func getTemplateFromDB(kodeAcara string) string {
	var isi string
	// Ambil template dari tb_template_pesan berdasarkan kode_acara
	err := dbConn.QueryRow("SELECT isi_template FROM tb_template_pesan WHERE kode_acara = ?", kodeAcara).Scan(&isi)
	if err != nil {
		// Fallback (Pesan Default) jika template untuk acara tsb belum diset di SQLite
		return "Yth Bapak/Ibu {{NAMA}},\n\nPembayaran Anda untuk acara:\n🖥️ \"{{ACARA}}\" telah kami terima.\n\n🗓️ Jadwal: {{TANGGAL}}\n⏰ Waktu: {{JAM}}\n\nTerima kasih,\nTim HPII Banten"
	}
	return isi
}

func renderMessage(p PesertaHPII) string {
	template := getTemplateFromDB(p.KodeAcara)
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

// processPendingQueue mengatur loop utama untuk antrean (Telah direfactor untuk menurunkan Cognitive Complexity)
func processPendingQueue() {
	now := time.Now()
	if !isSendingWindow(now) {
		return // Belum masuk jam operasional, abaikan
	}

	rows, err := dbConn.Query("SELECT id_server, nama, no_hp, pesan, COALESCE(retry_count, 0) FROM peserta_sinkron WHERE status_kirim = ? AND jam_kirim <= ?", statusPending, now)
	if err != nil {
		log.Printf("❌ Gagal membaca antrean pesan pending: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var job pendingJob
		if err := rows.Scan(&job.IDServer, &job.Nama, &job.NoHp, &job.Pesan, &job.RetryCount); err != nil {
			continue
		}
		processSinglePendingJob(job)
	}
}

// processSinglePendingJob mengeksekusi pesan WA individual dari antrean pending
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
	resp, err := waClient.SendMessage(waCtx, target, &waProto.Message{Conversation: proto.String(job.Pesan)})
	waCancel()

	if err != nil {
		handleFailedPendingJob(job, err)
		return
	}

	// Update database SQLite ke TERKIRIM
	_, dbErr := dbConn.Exec("UPDATE peserta_sinkron SET status_kirim = ?, jam_kirim = ?, retry_count = ? WHERE id_server = ?", statusTerkirim, time.Now(), job.RetryCount, job.IDServer)
	if dbErr != nil {
		log.Printf("⚠️ Gagal update status %s ke %s di DB lokal: %v", statusPending, statusTerkirim, dbErr)
	}

	saveAndBroadcast(targetJID, "HPII Banten", job.Pesan, true, "sent", time.Now(), resp.ID)
	time.Sleep(10 * time.Second) // Jeda antar pesan agar tidak terkena ban WhatsApp
}

// handleFailedPendingJob mengatur logika batas percobaan gagal (maks. 3 kali)
func handleFailedPendingJob(job pendingJob, sendErr error) {
	job.RetryCount++
	log.Printf("❌ Gagal mengirim WA pending ke %s (Percobaan %d/3): %v", job.NoHp, job.RetryCount, sendErr)

	if job.RetryCount >= 3 {
		// Batas maksimum percobaan tercapai, tandai sebagai FAILED
		_, dbErr := dbConn.Exec("UPDATE peserta_sinkron SET status_kirim = ?, retry_count = ? WHERE id_server = ?", statusFailed, job.RetryCount, job.IDServer)
		if dbErr != nil {
			log.Printf("⚠️ Gagal update status %s ke %s di DB lokal: %v", statusPending, statusFailed, dbErr)
		}
		log.Printf("⛔ Pesan ke %s dihentikan permanen (Status %s) setelah 3 kali gagal.", job.NoHp, statusFailed)
	} else {
		// Simpan increment retry_count tapi tetap PENDING untuk dicoba lagi nanti
		_, dbErr := dbConn.Exec("UPDATE peserta_sinkron SET retry_count = ? WHERE id_server = ?", job.RetryCount, job.IDServer)
		if dbErr != nil {
			log.Printf("⚠️ Gagal update retry_count di DB lokal: %v", dbErr)
		}
	}
}

func fetchAndProcess(apiURL string) {
	// Safety layer, seharusnya sudah terhenti di triggerAutoBroadcast
	if waClient == nil || !waClient.IsConnected() || waClient.Store == nil || waClient.Store.ID == nil {
		return
	}

	pesertaList, err := fetchPesertaFromAPI(apiURL)
	if err != nil {
		log.Printf("❌ Gagal Fetch API: %v", err)
		return
	}

	if len(pesertaList) == 0 {
		return
	}

	log.Printf("⏳ Ditemukan %d tiket peserta lunas baru. Mulai sinkronisasi...", len(pesertaList))

	for _, peserta := range pesertaList {
		processSinglePeserta(peserta, apiURL)
	}
	log.Println("🎉 Siklus sinkronisasi API selesai.")
}

func fetchPesertaFromAPI(apiURL string) ([]PesertaHPII, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("gagal buat request HTTP: %v", err)
	}

	// Gunakan kredensial .env
	req.SetBasicAuth(hpiiApiUser, hpiiApiPass)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gagal hubungi server PHP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server PHP merespons dengan status error: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gagal membaca body respons: %v", err)
	}

	var pesertaList []PesertaHPII
	if err := json.Unmarshal(body, &pesertaList); err != nil {
		return nil, fmt.Errorf("JSON Error: %v", err)
	}

	return pesertaList, nil
}

func processSinglePeserta(peserta PesertaHPII, apiURL string) {
	cleanPhone := formatPhoneNumber(peserta.NoHP)
	targetJID := cleanPhone
	if !strings.Contains(targetJID, "@") {
		targetJID += waSuffix
	}
	target, err := types.ParseJID(targetJID)
	if err != nil {
		log.Printf("❌ Nomor tidak valid (%s) untuk %s", cleanPhone, peserta.Nama)
		return
	}

	// Render pesan menggunakan template SQLite
	pesan := renderMessage(peserta)
	now := time.Now()

	var statusKirim string
	var jamKirim time.Time

	// Update is_sync di Server PHP terlebih dahulu agar tidak ditarik dobel
	if markAsSyncedPHP(peserta.ID, apiURL) {

		// Logika Pengiriman berdasarkan Jam Operasional
		if isSendingWindow(now) {
			statusKirim = statusTerkirim
			jamKirim = now

			log.Printf("Mengeksekusi pengiriman WA ke: %s (%s)", peserta.Nama, cleanPhone)
			waCtx, waCancel := context.WithTimeout(context.Background(), 20*time.Second)
			resp, errWa := waClient.SendMessage(waCtx, target, &waProto.Message{Conversation: proto.String(pesan)})
			waCancel()

			if errWa != nil {
				log.Printf("❌ Gagal mengirim WA ke %s: %v", cleanPhone, errWa)
				statusKirim = statusFailed // Tandai gagal di db lokal jika kirim WA bermasalah secara langsung saat fetch
			} else {
				saveAndBroadcast(targetJID, "HPII Banten", pesan, true, "sent", now, resp.ID)
			}
			time.Sleep(10 * time.Second) // Jeda 10 detik antar pesan

		} else {
			// Masuk Antrean (Ditunda karena sedang jam malam)
			statusKirim = statusPending
			jamKirim = calculateNextSendTime(now)
			log.Printf("🌙 Di luar jam operasional. WA untuk %s (%s) masuk antrean %s (Jadwal: %v)", peserta.Nama, cleanPhone, statusPending, jamKirim.Format("15:04"))
		}

		// Simpan riwayat peserta ke SQLite (Menggunakan waktu Golang lokal untuk waktu_sinkron, nilai awal retry_count = 0)
		_, errDb := dbConn.Exec("INSERT OR REPLACE INTO peserta_sinkron (id_server, nama, no_hp, pesan, status_kirim, jam_kirim, waktu_sinkron, retry_count) VALUES (?, ?, ?, ?, ?, ?, ?, 0)",
			peserta.ID, peserta.Nama, cleanPhone, pesan, statusKirim, jamKirim, now)
		if errDb != nil {
			log.Printf("⚠️ WA diproses, tapi gagal menyimpan ke history SQLite lokal (ID: %d): %v", peserta.ID, errDb)
		}

	} else {
		log.Printf("⚠️ Gagal update status is_sync di server PHP (ID: %d). Membatalkan eksekusi WA untuk mencegah pesan dobel.", peserta.ID)
	}
}

func markAsSyncedPHP(id int, apiURL string) bool {
	payload, _ := json.Marshal(map[string]int{"id": id})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(payload))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(hpiiApiUser, hpiiApiPass)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Jika API mengembalikan status 200 OK, berarti berhasil update ke is_sync = 1
	return resp.StatusCode == http.StatusOK
}
