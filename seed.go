package main

import (
	"log"
)

// RunSeeders berfungsi untuk mengisi data awal (master data) ke database.
// Menggunakan metode INSERT OR REPLACE agar aman dijalankan berulang kali (Idempotent).
func RunSeeders() {
	log.Println("🌱 Memulai proses injeksi Seed Data ke database...")

	// -------------------------------------------------------------------------
	// 1. SEED PENGATURAN SISTEM
	// -------------------------------------------------------------------------
	_, err := dbConn.Exec(`
		INSERT OR REPLACE INTO pengaturan_sistem (id, jam_buka, jam_tutup) 
		VALUES (1, '00:00', '23:59');
	`)
	if err != nil {
		log.Printf("❌ Gagal seed tabel pengaturan_sistem: %v", err)
	} else {
		log.Println("✅ [1/3] Seed pengaturan_sistem berhasil (00:00 - 23:59).")
	}

	// -------------------------------------------------------------------------
	// 2. SEED API ENDPOINTS
	// -------------------------------------------------------------------------
	// Kita definisikan ID secara eksplisit (1 dan 2) agar REPLACE bisa bekerja
	// dan mencegah duplikasi data jika seeder dijalankan lebih dari sekali.
	_, err = dbConn.Exec(`
		INSERT OR REPLACE INTO api_endpoints 
		(id, nama_sistem, url, auth_type, username, password, token, cron_expression, last_sync_time, is_active) 
		VALUES 
		(1, 'HPII Banten', 'https://hpiibanten.org/api_peserta.php', 'BASIC', 'HP11User', 'HP11Passwd', NULL, '*/10 * * * * *', '2026-05-17 05:28:51', 1),
		(2, 'e-Kredensial', 'http://127.0.0.1:7007/api/v1/wabot/reminder', 'HEADER', NULL, NULL, 'akang-hendi-secret-wabot-key', '*/10 * * * * *', '2026-05-17 05:28:51', 1);
	`)
	if err != nil {
		log.Printf("❌ Gagal seed tabel api_endpoints: %v", err)
	} else {
		log.Println("✅ [2/3] Seed api_endpoints berhasil (HPII Banten & e-Kredensial).")
	}

	// -------------------------------------------------------------------------
	// 3. SEED TEMPLATE PESAN
	// -------------------------------------------------------------------------
	_, err = dbConn.Exec(`
		INSERT OR REPLACE INTO tb_template_pesan (kode_acara, status, isi_template) 
		VALUES 
		('WB-260523', 'BELUM', 'Yth Bapak/Ibu {{NAMA}},

Kami menginformasikan bahwa pendaftaran Anda untuk acara:
🖥️ *{{ACARA}}* Telah kami terima ke dalam antrean sistem.

Mohon kesabaran menunggu. Proses verifikasi maksimal 1 x 24 jam.

Terima kasih,
*Tim IT HPII Banten*'),

		('WB-260523', 'LUNAS', 'Yth Bapak/Ibu {{NAMA}},

Selamat! Pendaftaran Anda untuk acara:
🖥️ *{{ACARA}}* Telah berhasil *DIKONFIRMASI (LUNAS)*.

🗓️ Tanggal: {{TANGGAL}}
⏰ Waktu: {{JAM}}

Berikut kami sampaikan link WAG untuk bergabung:
https://chat.whatsapp.com/LoxhTLK1bqn4vL2nxiTNtv?mode=gi_t

Terima kasih,
*Tim IT HPII Banten*'),

		('KREDENSIAL-REMINDER', 'WARNING', 'Yth. Sejawat {{NAMA}},

Pemberitahuan dari Sistem e-Kredensial:
Dokumen *{{DOKUMEN}}* Anda akan memasuki masa kedaluwarsa dalam *{{SISA_HARI}} hari*.

Mohon segera melakukan pembaruan dokumen melalui portal e-Kredensial untuk memastikan kelengkapan berkas Anda tetap berlaku.

Terima kasih,
*Sub-Komite Kredensial Keperawatan*');
	`)
	if err != nil {
		log.Printf("❌ Gagal seed tabel tb_template_pesan: %v", err)
	} else {
		log.Println("✅ [3/3] Seed tb_template_pesan berhasil.")
	}

	log.Println("🎉 Seluruh Master Data telah berhasil diinjeksi ke Database!")
}
