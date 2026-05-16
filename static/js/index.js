 document.addEventListener('alpine:init', () => {
        Alpine.data('globalApp', () => ({
            socket: null,
            reconnectTimer: null,
            toastMsg: '',
            toastTimer: null,
            activeTab: 'semua', 

            init() {
                this.connectWS();

                window.addEventListener('ws-outbound', (e) => {
                    if (this.socket && this.socket.readyState === WebSocket.OPEN) {
                        this.socket.send(JSON.stringify(e.detail));
                    }
                });
            },

            showToast(msg) {
                this.toastMsg = msg;
                clearTimeout(this.toastTimer);
                this.toastTimer = setTimeout(() => {
                    this.toastMsg = '';
                }, 3000);
            },

            setTab(tab) {
                this.activeTab = tab;
            },

            filterChat(isGroupStr) {
                if (this.activeTab === 'semua') return true;
                if (this.activeTab === 'grup') return isGroupStr === 'true';
                return false; 
            },
            
            connectWS() {
                this.socket = new WebSocket(`ws://${window.location.host}/ws`);

                this.socket.onmessage = (e) => {
                    const msg = JSON.parse(e.data);

                    if (msg.type === "logout") {
                        console.warn("Sesi diputus dari perangkat utama. Mengalihkan ke halaman login...");
                        window.location.href = '/login';
                        return;
                    }

                    window.dispatchEvent(new CustomEvent('ws-message', { detail: msg }));
                    
                    if (msg.content !== "[STATUS_UPDATE]" && msg.content !== "[LOGOUT]") {
                        this.updateSidebar(msg);
                    }
                };

                this.socket.onerror = (err) => {
                    console.warn('WS error terdeteksi. Menutup koneksi...', err);
                    this.socket.close();
                };

                this.socket.onclose = () => {
                    clearTimeout(this.reconnectTimer);
                    this.reconnectTimer = setTimeout(() => {
                        this.connectWS();
                    }, 3000);
                };
            },

            updateSidebar(msg) {
                const chatList = document.querySelector('.chat-list');
                const chatItem = document.querySelector(`.chat-item[data-jid="${msg.chat_jid}"]`);
                
                if (chatItem) {
                    // Update kontak yang sudah ada
                    const preview = chatItem.querySelector('.chat-item-preview');
                    const time = chatItem.querySelector('.chat-item-time');
                    
                    if (preview) preview.textContent = msg.content;
                    if (time) time.textContent = msg.display_time || "Baru saja";
                    
                    chatList.prepend(chatItem);
                } else {
                    // PERBAIKAN LOGIKA: Buat elemen kontak baru jika chat belum ada di daftar
                    
                    // Hilangkan pesan "Belum ada pesan yang tersimpan"
                    const emptyState = document.getElementById('empty-state');
                    if (emptyState) emptyState.remove();

                    const isGroup = msg.chat_jid.includes('@g.us');
                    const iconClass = isGroup ? 'fa-users' : 'fa-user';
                    
                    // Ambil nama dari pesan masuk, jika kosong gunakan nomor telepon/JID aslinya
                    const safeName = msg.sender_name || msg.chat_jid.split('@')[0];

                    const newItem = document.createElement('div');
                    newItem.className = 'chat-item';
                    newItem.setAttribute('data-jid', msg.chat_jid);
                    
                    newItem.innerHTML = `
                        <div class="avatar">
                            <i class="fa-solid ${iconClass}"></i>
                        </div>
                        <div class="chat-item-body">
                            <div class="chat-item-top">
                                <span class="chat-item-name">${safeName}</span>
                                <span class="chat-item-time">${msg.display_time || "Baru saja"}</span>
                            </div>
                            <div class="chat-item-bottom">
                                <span class="chat-item-preview">${msg.content}</span>
                            </div>
                        </div>
                    `;

                    // Event listener klik agar bisa membuka jendela chat
                    newItem.addEventListener('click', function() {
                        window.dispatchEvent(new CustomEvent('open-chat', { 
                            detail: {jid: msg.chat_jid, name: safeName} 
                        }));
                        document.querySelectorAll('.chat-item').forEach(i => i.classList.remove('active'));
                        this.classList.add('active');
                    });

                    // Sisipkan elemen kontak baru di posisi teratas
                    chatList.prepend(newItem);
                }
            }
        }));
    });