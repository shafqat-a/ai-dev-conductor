package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/shafqat-a/ai-dev-conductor/api"
	"github.com/shafqat-a/ai-dev-conductor/config"
	"github.com/shafqat-a/ai-dev-conductor/internal/auth"
	"github.com/shafqat-a/ai-dev-conductor/internal/session"
	"github.com/shafqat-a/ai-dev-conductor/internal/store"
	"github.com/shafqat-a/ai-dev-conductor/internal/ws"
)

//go:embed web/templates/*.html
var templateFS embed.FS

//go:embed web/static/*
var staticFS embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	authSvc, err := auth.NewAuthService(cfg.Password)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	sessionStore := auth.NewSessionStore()
	loginLimiter := auth.NewRateLimiter(cfg.LoginMaxAttempts, cfg.LoginWindow, cfg.LoginLockout)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	metaStore, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	sessionMgr := session.NewManager(cfg.Shell, cfg.DataDir, metaStore, cfg.IdleTimeout, cfg.MaxSessions, true)

	// Parse templates — use fs.Sub to strip prefix so template names are just "login.html" etc.
	templateSub, _ := fs.Sub(templateFS, "web/templates")
	tmpl := template.Must(template.ParseFS(templateSub, "*.html"))

	// pageData is handed to every HTML template so client-side asset, API and
	// WebSocket URLs can be prefixed with the base path when the app is served
	// under a reverse-proxy subpath (e.g. /terminaltest).
	pageData := map[string]any{"BasePath": cfg.BasePath}

	staticSub, _ := fs.Sub(staticFS, "web/static")

	// mountRoutes registers every route relative to the mount point. It is mounted
	// either at the host root or under cfg.BasePath; http.StripPrefix uses the full
	// public path because r.URL.Path still carries the base prefix.
	mountRoutes := func(r chi.Router) {
		// Static files
		r.Handle("/static/*", http.StripPrefix(cfg.BasePath+"/static/", http.FileServer(http.FS(staticSub))))

		// Public routes
		r.Get("/api/health", api.HandleHealthCheck())
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			tmpl.ExecuteTemplate(w, "login.html", pageData)
		})
		r.Post("/api/login", api.HandleLogin(authSvc, sessionStore, loginLimiter, cfg.SessionTimeout, cfg.BasePath))

		// Public read-only share link: viewer page + its unauthenticated WS attach.
		// The token in the URL is the only secret; both routes validate it server-side.
		r.Get("/s/{token}", func(w http.ResponseWriter, r *http.Request) {
			token := chi.URLParam(r, "token")
			st := sessionMgr.Store()
			valid := false
			if st != nil {
				sum := sha256.Sum256([]byte(token))
				if _, _, ok, err := st.RedeemShare(sum[:], time.Now().Unix()); err == nil && ok {
					valid = true
				}
			}
			if !valid {
				w.WriteHeader(http.StatusNotFound)
				tmpl.ExecuteTemplate(w, "share_invalid.html", pageData)
				return
			}
			tmpl.ExecuteTemplate(w, "share.html", pageData)
		})
		r.Get("/ws/share/{token}", ws.HandleShareWebSocket(sessionMgr))

		// Protected routes
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(sessionStore, cfg.BasePath))

			r.Get("/terminal", func(w http.ResponseWriter, r *http.Request) {
				tmpl.ExecuteTemplate(w, "terminal.html", pageData)
			})

			r.Get("/api/sessions", api.HandleListSessions(sessionMgr))
			r.Post("/api/sessions", api.HandleCreateSession(sessionMgr))
			r.Put("/api/sessions/{id}", api.HandleRenameSession(sessionMgr))
			r.Delete("/api/sessions/{id}", api.HandleDeleteSession(sessionMgr))
			r.Post("/api/sessions/{id}/share", api.HandleMintShare(sessionMgr, cfg.PublicURL, cfg.ShareTTL))
			r.Get("/api/sessions/{id}/shares", api.HandleListShares(sessionMgr))
			r.Delete("/api/shares/{id}", api.HandleRevokeShare(sessionMgr))
			r.Get("/ws/{id}", ws.HandleWebSocket(sessionMgr))
		})
	}

	// Router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	if cfg.BasePath == "" {
		mountRoutes(r)
	} else {
		// Bare prefix with no trailing slash → send the browser to the login page.
		r.Get(cfg.BasePath, func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, cfg.BasePath+"/", http.StatusMovedPermanently)
		})
		r.Route(cfg.BasePath, mountRoutes)
	}

	// Server with graceful shutdown
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Write PID file if configured
	if cfg.PIDFile != "" {
		if err := os.WriteFile(cfg.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			log.Fatalf("pid file: %v", err)
		}
		log.Printf("PID file: %s", cfg.PIDFile)
	}

	go func() {
		log.Printf("Shell: %s", cfg.Shell)
		log.Printf("Session backend: tmux (%s) — sessions survive restart", cfg.TmuxBin)
		log.Printf("Listening on %s", cfg.ListenAddr)
		for _, addr := range getAccessURLs(cfg.ListenAddr) {
			log.Printf("  -> %s%s/", addr, cfg.BasePath)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Wait for interrupt; ignore SIGHUP so we don't crash when backgrounded
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	signal.Ignore(syscall.SIGHUP)
	<-quit

	log.Println("Shutting down...")
	sessionMgr.CloseAll()
	if err := metaStore.Close(); err != nil {
		log.Printf("store: close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// Remove PID file on clean shutdown
	if cfg.PIDFile != "" {
		os.Remove(cfg.PIDFile)
	}

	log.Println("Server stopped")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Session-Token")
			w.Header().Set("Access-Control-Max-Age", "3600")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func getAccessURLs(listenAddr string) []string {
	_, port, _ := net.SplitHostPort(listenAddr)
	if port == "" {
		port = "8080"
	}

	var urls []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{fmt.Sprintf("http://%s", listenAddr)}
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := addr.(*net.IPNet).IP
			if ip.To4() == nil {
				continue // skip IPv6
			}
			urls = append(urls, fmt.Sprintf("http://%s:%s", ip.String(), port))
		}
	}

	if len(urls) == 0 {
		return []string{fmt.Sprintf("http://localhost:%s", port)}
	}

	// Always include localhost
	hasLocal := false
	for _, u := range urls {
		if strings.Contains(u, "127.0.0.1") {
			hasLocal = true
			break
		}
	}
	if !hasLocal {
		urls = append([]string{fmt.Sprintf("http://localhost:%s", port)}, urls...)
	}

	return urls
}
