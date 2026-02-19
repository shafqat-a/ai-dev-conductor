class TerminalManager {
    constructor() {
        // Current connection state
        this.currentServerId = null;
        this.currentSessionId = null;
        this.ws = null;
        this.term = null;
        this.fitAddon = null;
        this.manualDisconnect = false;
        this.reconnectAttempts = 0;
        this.reconnectTimer = null;
        this.maxReconnectAttempts = 20;

        // Server management
        this.servers = this.loadServers();

        // DOM elements
        this.sessionListEl = document.getElementById('session-list');
        this.placeholderEl = document.getElementById('placeholder');
        this.containerEl = document.getElementById('terminal-container');

        document.getElementById('btn-new-session').addEventListener('click', () => this.createSession());
        document.getElementById('btn-add-server').addEventListener('click', () => this.addServer());
        window.addEventListener('resize', () => this.handleResize());

        this.loadAllSessions();
    }

    // --- Server Management ---

    loadServers() {
        const stored = localStorage.getItem('ai_conductor_servers');
        if (stored) {
            const servers = JSON.parse(stored);
            if (!servers.find(s => s.isLocal)) {
                servers.unshift({ id: 'local', name: 'Local', url: '', token: null, isLocal: true, connected: true });
            }
            return servers;
        }
        return [{ id: 'local', name: 'Local', url: '', token: null, isLocal: true, connected: true }];
    }

    saveServers() {
        localStorage.setItem('ai_conductor_servers', JSON.stringify(this.servers));
    }

    getServerBaseUrl(server) {
        if (server.isLocal || !server.url) return '';
        return server.url.replace(/\/$/, '');
    }

    getServerById(serverId) {
        return this.servers.find(s => s.id === serverId);
    }

    async fetchFromServer(server, path, options = {}) {
        const baseUrl = this.getServerBaseUrl(server);
        const url = baseUrl + path;

        if (server.isLocal) {
            return fetch(url, options);
        }

        const headers = { ...(options.headers || {}) };
        if (server.token) {
            headers['X-Session-Token'] = server.token;
        }
        return fetch(url, { ...options, headers, credentials: 'omit' });
    }

    async addServer() {
        const name = prompt('Server name:');
        if (!name) return;
        const url = prompt('Server URL (e.g. http://192.168.1.50:8080):');
        if (!url) return;

        const server = {
            id: Math.random().toString(36).slice(2, 10),
            name: name.trim(),
            url: url.trim().replace(/\/$/, ''),
            token: null,
            isLocal: false,
            connected: false,
        };

        const authenticated = await this.authenticateServer(server);
        if (!authenticated) return;

        this.servers.push(server);
        this.saveServers();
        await this.loadAllSessions();
    }

    async authenticateServer(server) {
        const password = prompt(`Password for ${server.name}:`);
        if (password === null) return false;

        try {
            const res = await fetch(server.url + '/api/login', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ password }),
                credentials: 'omit',
            });
            if (res.ok) {
                const data = await res.json();
                server.token = data.token;
                server.connected = true;
                this.saveServers();
                return true;
            } else {
                alert('Authentication failed');
                return false;
            }
        } catch (err) {
            alert('Cannot connect to ' + server.name + ': ' + err.message);
            return false;
        }
    }

    removeServer(serverId) {
        // Disconnect if currently connected to a session on this server
        if (this.currentServerId === serverId) {
            this.disconnect();
            this.showPlaceholder();
        }
        this.servers = this.servers.filter(s => s.id !== serverId);
        this.saveServers();
        this.loadAllSessions();
    }

    // --- Session Loading ---

    async loadAllSessions() {
        const results = await Promise.allSettled(
            this.servers.map(async (server) => {
                try {
                    const res = await this.fetchFromServer(server, '/api/sessions');
                    if (res.status === 401) {
                        if (server.isLocal) {
                            window.location.href = '/';
                            return [];
                        }
                        server.connected = false;
                        this.saveServers();
                        return [];
                    }
                    const sessions = await res.json();
                    server.connected = true;
                    return sessions.map(s => ({ ...s, serverId: server.id }));
                } catch {
                    server.connected = false;
                    return [];
                }
            })
        );

        const allSessions = results.flatMap(r => r.status === 'fulfilled' ? r.value : []);
        this.renderSessionList(allSessions);
    }

    // --- Rendering ---

    renderSessionList(sessions) {
        this.sessionListEl.innerHTML = '';

        // Group sessions by server
        const grouped = {};
        this.servers.forEach(s => { grouped[s.id] = { server: s, sessions: [] }; });
        sessions.forEach(s => {
            if (grouped[s.serverId]) {
                grouped[s.serverId].sessions.push(s);
            }
        });

        for (const [serverId, group] of Object.entries(grouped)) {
            const server = group.server;

            // Server group header
            const header = document.createElement('div');
            header.className = 'server-group-header';
            const statusClass = server.connected !== false ? 'connected' : 'disconnected';
            header.innerHTML =
                '<span class="server-status ' + statusClass + '"></span>' +
                '<span class="server-name">' + this.escapeHtml(server.name) + '</span>';

            if (!server.isLocal) {
                const menuBtn = document.createElement('button');
                menuBtn.className = 'btn-server-menu';
                menuBtn.title = 'Server options';
                menuBtn.innerHTML = '&#8942;';
                menuBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    this.showServerMenu(server, menuBtn);
                });
                header.appendChild(menuBtn);
            }

            this.sessionListEl.appendChild(header);

            // Sessions under this server
            group.sessions.forEach(s => {
                const isActive = this.currentServerId === serverId && this.currentSessionId === s.id;
                const item = document.createElement('div');
                item.className = 'session-item' + (isActive ? ' active' : '');
                item.dataset.serverId = serverId;
                item.dataset.sessionId = s.id;

                const nameSpan = document.createElement('span');
                nameSpan.className = 'session-name';
                nameSpan.title = s.createdAt;
                nameSpan.textContent = s.name || s.id;
                nameSpan.addEventListener('click', () => this.connectToSession(serverId, s.id));

                const renameBtn = document.createElement('button');
                renameBtn.className = 'btn-rename';
                renameBtn.title = 'Rename session';
                renameBtn.innerHTML = '&#9998;';
                renameBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    this.renameSession(serverId, s.id, s.name || s.id);
                });

                const deleteBtn = document.createElement('button');
                deleteBtn.className = 'btn-delete';
                deleteBtn.title = 'Delete session';
                deleteBtn.innerHTML = '&times;';
                deleteBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    this.deleteSession(serverId, s.id);
                });

                item.appendChild(nameSpan);
                item.appendChild(renameBtn);
                item.appendChild(deleteBtn);
                this.sessionListEl.appendChild(item);
            });
        }
    }

    showServerMenu(server, anchorEl) {
        document.querySelectorAll('.server-menu').forEach(el => el.remove());

        const menu = document.createElement('div');
        menu.className = 'server-menu';

        const reconnectOpt = document.createElement('div');
        reconnectOpt.className = 'server-menu-item';
        reconnectOpt.textContent = 'Reconnect';
        reconnectOpt.addEventListener('click', async () => {
            menu.remove();
            const ok = await this.authenticateServer(server);
            if (ok) await this.loadAllSessions();
        });

        const removeOpt = document.createElement('div');
        removeOpt.className = 'server-menu-item danger';
        removeOpt.textContent = 'Remove';
        removeOpt.addEventListener('click', () => {
            menu.remove();
            this.removeServer(server.id);
        });

        menu.appendChild(reconnectOpt);
        menu.appendChild(removeOpt);

        // Position near the anchor
        anchorEl.style.position = 'relative';
        anchorEl.parentElement.style.position = 'relative';
        anchorEl.parentElement.appendChild(menu);

        const closeHandler = (e) => {
            if (!menu.contains(e.target)) {
                menu.remove();
                document.removeEventListener('click', closeHandler);
            }
        };
        setTimeout(() => document.addEventListener('click', closeHandler), 0);
    }

    // --- Session CRUD ---

    async createSession() {
        const connectedServers = this.servers.filter(s => s.connected !== false);

        let targetServer;
        if (connectedServers.length === 0) {
            alert('No servers connected');
            return;
        } else if (connectedServers.length === 1) {
            targetServer = connectedServers[0];
        } else {
            targetServer = await this.showServerPicker(connectedServers);
            if (!targetServer) return;
        }

        try {
            const res = await this.fetchFromServer(targetServer, '/api/sessions', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({}),
            });
            if (res.status === 401) {
                if (targetServer.isLocal) {
                    window.location.href = '/';
                } else {
                    await this.authenticateServer(targetServer);
                }
                return;
            }
            const data = await res.json();
            await this.loadAllSessions();
            this.connectToSession(targetServer.id, data.id);
        } catch (err) {
            console.error('Failed to create session:', err);
        }
    }

    showServerPicker(servers) {
        return new Promise((resolve) => {
            document.querySelectorAll('.server-picker').forEach(el => el.remove());

            const picker = document.createElement('div');
            picker.className = 'server-picker';
            servers.forEach(s => {
                const opt = document.createElement('div');
                opt.className = 'server-picker-item';
                opt.textContent = s.name;
                opt.addEventListener('click', () => {
                    picker.remove();
                    resolve(s);
                });
                picker.appendChild(opt);
            });

            const btn = document.getElementById('btn-new-session');
            btn.parentElement.style.position = 'relative';
            btn.parentElement.appendChild(picker);

            const closeHandler = (e) => {
                if (!picker.contains(e.target) && e.target !== btn) {
                    picker.remove();
                    document.removeEventListener('click', closeHandler);
                    resolve(null);
                }
            };
            setTimeout(() => document.addEventListener('click', closeHandler), 0);
        });
    }

    async renameSession(serverId, sessionId, currentName) {
        const newName = prompt('Rename session:', currentName);
        if (newName === null || newName.trim() === '') return;

        const server = this.getServerById(serverId);
        if (!server) return;

        try {
            const res = await this.fetchFromServer(server, '/api/sessions/' + sessionId, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ name: newName.trim() }),
            });
            if (res.ok) {
                await this.loadAllSessions();
            }
        } catch (err) {
            console.error('Failed to rename session:', err);
        }
    }

    async deleteSession(serverId, sessionId) {
        const server = this.getServerById(serverId);
        if (!server) return;

        try {
            await this.fetchFromServer(server, '/api/sessions/' + sessionId, { method: 'DELETE' });
            if (this.currentServerId === serverId && this.currentSessionId === sessionId) {
                this.disconnect();
                this.showPlaceholder();
            }
            await this.loadAllSessions();
        } catch (err) {
            console.error('Failed to delete session:', err);
        }
    }

    // --- Terminal Connection ---

    connectToSession(serverId, sessionId) {
        this.disconnect();
        this.currentServerId = serverId;
        this.currentSessionId = sessionId;
        this.manualDisconnect = false;
        this.reconnectAttempts = 0;

        // Show terminal container
        this.placeholderEl.style.display = 'none';
        this.containerEl.style.display = 'block';
        this.containerEl.innerHTML = '';

        // Update active state in sidebar
        this.sessionListEl.querySelectorAll('.session-item').forEach(el => {
            const isActive = el.dataset.serverId === serverId && el.dataset.sessionId === sessionId;
            el.classList.toggle('active', isActive);
        });

        // Create terminal
        this.term = new Terminal({
            cursorBlink: true,
            fontSize: 14,
            fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', Menlo, monospace",
            theme: {
                background: '#1a1b26',
                foreground: '#c0caf5',
                cursor: '#c0caf5',
                selectionBackground: '#33467c',
                black: '#15161e',
                red: '#f7768e',
                green: '#9ece6a',
                yellow: '#e0af68',
                blue: '#7aa2f7',
                magenta: '#bb9af7',
                cyan: '#7dcfff',
                white: '#a9b1d6',
                brightBlack: '#414868',
                brightRed: '#f7768e',
                brightGreen: '#9ece6a',
                brightYellow: '#e0af68',
                brightBlue: '#7aa2f7',
                brightMagenta: '#bb9af7',
                brightCyan: '#7dcfff',
                brightWhite: '#c0caf5',
            }
        });

        this.fitAddon = new FitAddon.FitAddon();
        this.term.loadAddon(this.fitAddon);
        this.term.loadAddon(new WebLinksAddon.WebLinksAddon());

        this.term.open(this.containerEl);
        this.fitAddon.fit();

        // Send terminal input to server
        this.term.onData((data) => {
            if (this.ws && this.ws.readyState === WebSocket.OPEN) {
                this.ws.send(JSON.stringify({ type: 'input', data: data }));
            }
        });

        // Handle binary data (e.g. image paste, non-UTF8 clipboard content)
        this.term.onBinary((data) => {
            if (this.ws && this.ws.readyState === WebSocket.OPEN) {
                const buffer = new Uint8Array(data.length);
                for (let i = 0; i < data.length; i++) {
                    buffer[i] = data.charCodeAt(i) & 0xff;
                }
                this.ws.send(buffer);
            }
        });

        this.openWebSocket(serverId, sessionId);
    }

    openWebSocket(serverId, sessionId) {
        const server = this.getServerById(serverId);
        if (!server) return;

        let wsUrl;
        if (server.isLocal) {
            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            wsUrl = protocol + '//' + window.location.host + '/ws/' + sessionId;
        } else {
            const url = new URL(server.url);
            const protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
            wsUrl = protocol + '//' + url.host + '/ws/' + sessionId + '?token=' + encodeURIComponent(server.token || '');
        }

        this.ws = new WebSocket(wsUrl);

        this.ws.onopen = () => {
            this.reconnectAttempts = 0;
            this.sendResize();
        };

        this.ws.onmessage = (event) => {
            try {
                const msg = JSON.parse(event.data);
                if (msg.type === 'output') {
                    this.term.write(msg.data);
                }
            } catch {
                // Ignore malformed messages
            }
        };

        this.ws.onclose = () => {
            if (this.manualDisconnect || this.currentSessionId !== sessionId || this.currentServerId !== serverId) {
                return;
            }
            this.attemptReconnect(serverId, sessionId);
        };

        this.ws.onerror = () => {
            // onclose will fire after this, reconnect handled there
        };
    }

    attemptReconnect(serverId, sessionId) {
        if (this.manualDisconnect || this.currentSessionId !== sessionId || this.currentServerId !== serverId) {
            return;
        }

        if (this.reconnectAttempts >= this.maxReconnectAttempts) {
            if (this.term) {
                this.term.write('\r\n\x1b[31m[Connection lost. Click to reconnect.]\x1b[0m\r\n');
                const disposable = this.term.onData(() => {
                    disposable.dispose();
                    this.reconnectAttempts = 0;
                    this.attemptReconnect(serverId, sessionId);
                });
            }
            return;
        }

        const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempts), 30000);
        this.reconnectAttempts++;

        if (this.term) {
            this.term.write('\r\n\x1b[33m[Reconnecting (' + this.reconnectAttempts + '/' + this.maxReconnectAttempts + ')...]\x1b[0m\r\n');
        }

        this.reconnectTimer = setTimeout(() => {
            if (this.manualDisconnect || this.currentSessionId !== sessionId || this.currentServerId !== serverId) {
                return;
            }
            this.openWebSocket(serverId, sessionId);
        }, delay);
    }

    disconnect() {
        this.manualDisconnect = true;
        if (this.reconnectTimer) {
            clearTimeout(this.reconnectTimer);
            this.reconnectTimer = null;
        }
        if (this.ws) {
            this.ws.close();
            this.ws = null;
        }
        if (this.term) {
            this.term.dispose();
            this.term = null;
            this.fitAddon = null;
        }
        this.currentSessionId = null;
        this.currentServerId = null;
    }

    showPlaceholder() {
        this.containerEl.style.display = 'none';
        this.containerEl.innerHTML = '';
        this.placeholderEl.style.display = 'flex';
        this.currentSessionId = null;
        this.currentServerId = null;
    }

    handleResize() {
        if (this.fitAddon && this.term) {
            this.fitAddon.fit();
            this.sendResize();
        }
    }

    sendResize() {
        if (this.ws && this.ws.readyState === WebSocket.OPEN && this.term) {
            this.ws.send(JSON.stringify({
                type: 'resize',
                cols: this.term.cols,
                rows: this.term.rows,
            }));
        }
    }

    // --- Utilities ---

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }
}

// Initialize
const manager = new TerminalManager();
