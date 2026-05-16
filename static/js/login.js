document.addEventListener('alpine:init', () => {
    Alpine.data('loginHandler', () => ({
        hasQR: false,
        currentQRText: '',
        qrElement: null,

        init() {
            console.log("[Sistem] Menginisialisasi handler login...");
            this.checkStatus();
            setInterval(() => this.checkStatus(), 2000);
        },

        checkStatus() {
            fetch('/api/status')
                .then(res => res.json())
                .then(data => {
                    if (data.logged_in) {
                        console.log("[Sistem] Sesi aktif ditemukan. Mengalihkan ke Dashboard...");
                        window.location.href = '/pesan';
                        return;
                    }
                    
                    // Jika belum login, tarik QR
                    this.fetchQR();
                })
                .catch(err => console.error('[Error] Gagal memeriksa status server:', err));
        },

        fetchQR() {
            fetch('/api/qr')
                .then(res => res.json())
                .then(data => {
                    if (data.qr && data.qr !== this.currentQRText) {
                        console.log("[Sistem] QR Code baru berhasil ditarik dari server.");
                        this.currentQRText = data.qr;
                        this.hasQR = true;
                        
                        // $nextTick memastikan DOM HTML (div qrcode) sudah tampil (tidak display:none) 
                        // sebelum library qrcode.js mencoba menggambar di dalamnya.
                        this.$nextTick(() => {
                            this.renderQR(this.currentQRText);
                        });
                    } else if (!data.qr && this.hasQR) {
                        // Reset state jika QR kedaluwarsa atau server meresetnya
                        this.hasQR = false;
                        this.currentQRText = '';
                    }
                })
                .catch(err => console.error('[Error] Gagal mengambil string QR:', err));
        },

        renderQR(text) {
            // Validasi keamanan: Pastikan script eksternal berhasil dimuat
            if (typeof QRCode === 'undefined') {
                console.error("[Fatal] Library QRCode gagal dimuat oleh browser. Cek koneksi internet atau pemblokir iklan (AdBlock).");
                return;
            }

            const container = document.getElementById('qrcode');
            if (!container) return;

            container.innerHTML = ''; // Bersihkan elemen canvas/img QR sebelumnya
            
            try {
                this.qrElement = new QRCode(container, {
                    text: text,
                    width: 264,
                    height: 264,
                    colorDark : "#111b21",
                    colorLight : "#ffffff",
                    correctLevel : QRCode.CorrectLevel.L
                });
            } catch (error) {
                console.error("[Error] Terjadi kesalahan saat mencoba menggambar QR Code:", error);
            }
        }
    }));
});