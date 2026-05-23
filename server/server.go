package server

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"padel-cli/storage"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	Bind      string
	Port      int
	BinaryPath string
	ConfigDir string

	mu           sync.Mutex
	templates    map[string]*template.Template
	db           *sql.DB
	logger       *log.Logger
	currentRun   *runProcess
	walletStatus walletStatus
}

func New(bind string, port int, binaryPath, configDir string, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	db, err := storage.OpenBookingsDB()
	if err != nil {
		return nil, fmt.Errorf("open bookings db: %w", err)
	}
	return &Server{
		Bind:       bind,
		Port:       port,
		BinaryPath: binaryPath,
		ConfigDir:  configDir,
		templates:  tmpl,
		db:         db,
		logger:     logger,
	}, nil
}

func (s *Server) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	addr := net.JoinHostPort(s.Bind, fmt.Sprintf("%d", s.Port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Printf("listening on http://%s", addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/audit", s.handleAudit)
	mux.HandleFunc("/run", s.handleRunPage)
	mux.HandleFunc("/run/start", s.handleRunStart)
	mux.HandleFunc("/run/stream", s.handleRunStream)
	mux.HandleFunc("/wallet/refresh", s.handleWalletRefresh)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func loadTemplates() (map[string]*template.Template, error) {
	funcMap := template.FuncMap{
		"formatLocal": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("Mon 2 Jan 15:04")
		},
		"formatPrice": func(amount float64) string {
			if amount == 0 {
				return ""
			}
			return fmt.Sprintf("$%.2f", amount)
		},
		"upper": strings.ToUpper,
	}
	pages := []string{"dashboard", "audit", "run"}
	out := make(map[string]*template.Template, len(pages))
	// Each page is its own template set containing layout + that page's
	// content. This avoids the global-name collision when several files
	// define {{define "content"}}.
	for _, page := range pages {
		tmpl, err := template.New(page).Funcs(funcMap).ParseFS(templateFS,
			"templates/layout.html",
			"templates/"+page+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		out[page] = tmpl
	}
	return out, nil
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	tmpl, ok := s.templates[page]
	if !ok {
		s.logger.Printf("unknown page template %q", page)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Printf("template %s render error: %v", page, err)
	}
}
