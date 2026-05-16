document.addEventListener('alpine:init', () => {
    Alpine.data('broadcastApp', () => ({
        isDragging: false,
        selectedFile: null,
        isLoading: false,
        alert: {
            show: false,
            type: '', 
            message: ''
        },

        handleDrop(e) {
            this.isDragging = false;
            if (e.dataTransfer.files.length > 0) {
                this.selectedFile = e.dataTransfer.files[0];
            }
        },

        handleFileSelect(e) {
            if (e.target.files.length > 0) {
                this.selectedFile = e.target.files[0];
            }
        },

        showAlert(type, msg) {
            this.alert.type = type;
            this.alert.message = msg;
            this.alert.show = true;
            
            setTimeout(() => {
                this.alert.show = false;
            }, 6000);
        },

        uploadFile() {
            if (!this.selectedFile) return;

            this.isLoading = true;
            this.alert.show = false;

            const formData = new FormData();
            formData.append('file_kontak', this.selectedFile);

            fetch('/api/v1/broadcast/upload', {
                method: 'POST',
                body: formData
            })
            .then(async response => {
                // Perbaikan Penanganan Error Server-Side
                let data = {};
                try {
                    data = await response.json();
                } catch (err) {
                    throw new Error('Format respon server rusak atau koneksi gagal.');
                }
                
                if (!response.ok) {
                    throw new Error(data.error || `Server merespon dengan kode ${response.status}`);
                }
                return data;
            })
            .then(data => {
                this.isLoading = false;
                if (data && data.success) {
                    this.showAlert('success', data.message);
                    this.selectedFile = null; 
                } else {
                    this.showAlert('danger', data ? data.error : 'Terjadi kesalahan tidak diketahui.');
                }
            })
            .catch(error => {
                this.isLoading = false;
                this.showAlert('danger', 'Gagal memproses file: ' + error.message);
                console.error('Upload error detail:', error);
            });
        }
    }));
});