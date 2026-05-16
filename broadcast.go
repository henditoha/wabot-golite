package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/extrame/xls"
	"github.com/gofiber/fiber/v2"
	"github.com/xuri/excelize/v2"
)

func SetupBroadcastRoutes(app *fiber.App) {
	app.Get("/broadcast", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html")
		return c.SendFile("./templates/broadcast.html")
	})
	app.Post("/api/v1/broadcast/upload", handleBroadcastUpload)
}

type BroadcastRecord struct {
	Nama  string
	NoHP  string
	Pesan string
}

func handleBroadcastUpload(c *fiber.Ctx) error {
	file, err := c.FormFile("file_kontak")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "Gagal membaca file upload"})
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext != ".csv" && ext != ".xlsx" && ext != ".xls" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "Format file tidak didukung. Gunakan CSV, XLS, atau XLSX."})
	}

	fileData, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": "Gagal membuka file: " + err.Error()})
	}
	defer fileData.Close()

	var records []BroadcastRecord

	log.Printf("📥 Mulai memproses file broadcast: %s", file.Filename)

	switch ext {
	case ".csv":
		records, err = parseCSVFile(fileData)
	case ".xlsx":
		records, err = parseXLSXFile(fileData)
	case ".xls":
		records, err = parseXLSFileSafe(fileData)
	}

	if err != nil {
		log.Printf("❌ Error Parsing File: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": "Gagal membaca isi dokumen: " + err.Error()})
	}

	if len(records) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "File kosong atau format kolom tidak sesuai (Pastikan ada data di kolom Nama, No HP, dan Pesan mulai dari baris ke-2)"})
	}

	log.Printf("✅ Selesai Parsing. Bersiap menyimpan %d kontak ke database...", len(records))

	insertedCount, errInsert := insertBroadcastToDB(records)
	if errInsert != nil {
		log.Printf("❌ Error Database Insert: %v", errInsert)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": "Gagal menyimpan ke database: " + errInsert.Error()})
	}

	log.Printf("🎉 Sukses! %d kontak masuk ke antrean.", insertedCount)

	return c.JSON(fiber.Map{
		"success": true,
		"message": fmt.Sprintf("Berhasil memasukkan %d kontak ke antrean broadcast.", insertedCount),
	})
}

// Helper 1: Mengambil sel dengan aman
func safeCol(row []string, index int) string {
	if index < len(row) {
		return strings.TrimSpace(row[index])
	}
	return ""
}

// Helper 2: Pembersih Nomor HP
func cleanBroadcastPhone(phone string) string {
	re := regexp.MustCompile(`[^0-9]`)
	phone = re.ReplaceAllString(phone, "")

	if strings.HasPrefix(phone, "0") {
		return "62" + phone[1:]
	}
	return phone
}

// -- FUNGSI PARSING FILE --

func parseCSVFile(fileData multipart.File) ([]BroadcastRecord, error) {
	var records []BroadcastRecord

	b, err := io.ReadAll(fileData)
	if err != nil {
		return nil, err
	}

	content := string(b)
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	firstLine := strings.SplitN(content, "\n", 2)[0]
	delimiter := rune(';')
	if strings.Count(firstLine, ",") > strings.Count(firstLine, ";") {
		delimiter = rune(',')
	}

	reader := csv.NewReader(bytes.NewReader([]byte(content)))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	lines, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	for i, line := range lines {
		if i == 0 {
			continue
		}

		nama := safeCol(line, 0)
		noHP := cleanBroadcastPhone(safeCol(line, 1))
		pesanRaw := safeCol(line, 2)

		if noHP != "" && pesanRaw != "" {
			pesanFinal := strings.ReplaceAll(pesanRaw, "{{NAMA}}", nama)
			records = append(records, BroadcastRecord{Nama: nama, NoHP: noHP, Pesan: pesanFinal})
		}
	}
	return records, nil
}

func parseXLSXFile(fileData multipart.File) ([]BroadcastRecord, error) {
	var records []BroadcastRecord
	f, err := excelize.OpenReader(fileData)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Aman dari error jika nama sheet bukan 'Sheet1'
	sheetList := f.GetSheetList()
	if len(sheetList) == 0 {
		return nil, fmt.Errorf("tidak ada sheet ditemukan dalam file Excel")
	}

	rows, err := f.GetRows(sheetList[0])
	if err != nil {
		return nil, err
	}

	for i, row := range rows {
		if i == 0 {
			continue
		}

		nama := safeCol(row, 0)
		noHP := cleanBroadcastPhone(safeCol(row, 1))
		pesanRaw := safeCol(row, 2)

		if noHP != "" && pesanRaw != "" {
			pesanFinal := strings.ReplaceAll(pesanRaw, "{{NAMA}}", nama)
			records = append(records, BroadcastRecord{Nama: nama, NoHP: noHP, Pesan: pesanFinal})
		}
	}
	return records, nil
}

func parseXLSFileSafe(fileData multipart.File) ([]BroadcastRecord, error) {
	var records []BroadcastRecord

	b, err := io.ReadAll(fileData)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(b)

	wb, err := xls.OpenReader(reader, "utf-8")
	if err != nil {
		return nil, err
	}

	sheet := wb.GetSheet(0)
	if sheet == nil {
		return nil, fmt.Errorf("sheet pertama tidak ditemukan")
	}

	for i := 1; i <= int(sheet.MaxRow); i++ {
		row := sheet.Row(i)
		if row == nil {
			continue
		}

		nama := strings.TrimSpace(row.Col(0))
		noHP := cleanBroadcastPhone(row.Col(1))
		pesanRaw := strings.TrimSpace(row.Col(2))

		if noHP != "" && pesanRaw != "" {
			pesanFinal := strings.ReplaceAll(pesanRaw, "{{NAMA}}", nama)
			records = append(records, BroadcastRecord{Nama: nama, NoHP: noHP, Pesan: pesanFinal})
		}
	}
	return records, nil
}

// -- FUNGSI DATABASE --

func insertBroadcastToDB(records []BroadcastRecord) (int, error) {
	// SOLUSI DEADLOCK: Dapatkan status waktu SEBELUM membuka transaksi database
	now := time.Now()
	var statusKirim string
	var jamKirim time.Time

	if isSendingWindow(now) {
		statusKirim = statusPending
		jamKirim = now
	} else {
		statusKirim = statusPending
		jamKirim = calculateNextSendTime(now)
	}

	// Sekarang baru aman untuk membuka transaksi (Locking)
	tx, err := dbConn.Begin()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare("INSERT INTO autoresponse (nama, no_hp, pesan, status_kirim, jam_kirim, waktu_sinkron, retry_count) VALUES (?, ?, ?, ?, ?, ?, 0)")
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, rec := range records {
		_, errExec := stmt.Exec(rec.Nama, rec.NoHP, rec.Pesan, statusKirim, jamKirim, now)
		if errExec == nil {
			inserted++
		}
	}

	if errCommit := tx.Commit(); errCommit != nil {
		return 0, errCommit
	}

	return inserted, nil
}
