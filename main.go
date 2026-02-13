package main

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	sessionMgr := session.NewManager(cfg.Shell, cfg.DataDir)

	// Parse templates — use fs.Sub to strip prefix so template names are just "login.html" etc.
	templateSub, _ := fs.Sub(templateFS, "web/templates")
	tmpl := template.Must(template.ParseFS(templateSub, "*.html"))

	// Router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	// Static files
	staticSub, _ := fs.Sub(staticFS, "web/static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Public routes
	r.Get("/api/health", api.HandleHealthCheck())
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "login.html", nil)
	})
	r.Post("/api/login", api.HandleLogin(authSvc, sessionStore, cfg.SessionTimeout))

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(sessionStore))

		r.Get("/terminal", func(w http.ResponseWriter, r *http.Request) {
			tmpl.ExecuteTemplate(w, "terminal.html", nil)
		})

		r.Get("/api/sessions", api.HandleListSessions(sessionMgr))
		r.Post("/api/sessions", api.HandleCreateSession(sessionMgr))
		r.Put("/api/sessions/{id}", api.HandleRenameSession(sessionMgr))
		r.Delete("/api/sessions/{id}", api.HandleDeleteSession(sessionMgr))
		r.Get("/ws/{id}", ws.HandleWebSocket(sessionMgr))
	})

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
		log.Printf("Listening on %s", cfg.ListenAddr)
		for _, addr := range getAccessURLs(cfg.ListenAddr) {
			log.Printf("  -> %s", addr)
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
