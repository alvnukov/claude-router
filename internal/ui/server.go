package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed all:dist
var assets embed.FS

// Secure protects the entire UI listener, including legacy raw downloads.
func Secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
			Error(w, 403, "Доступ разрешён только через локальный адрес")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !SameOrigin(r) {
			Error(w, 403, "Запрос с другого сайта запрещён")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func SameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return err == nil && u.Scheme == scheme && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}
	return true
}
func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]string{"error": message})
}

func Mount(mux *http.ServeMux, b Backend) {
	mountPrivacy(mux, b)
	files, _ := fs.Sub(assets, "dist")
	serve := http.FileServerFS(files)
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		serve.ServeHTTP(w, r)
	}))
	page := func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(files, "index.html")
		if err != nil {
			Error(w, 503, "Интерфейс ещё не собран")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
	for _, path := range []string{"/{$}", "/requests", "/requests/{id}", "/routes", "/connections", "/settings", "/privacy"} {
		mux.HandleFunc("GET "+path, page)
	}
	mux.HandleFunc("GET /api/ui/state", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, b.State(r.Context())) })
	mux.HandleFunc("GET /api/ui/requests", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, b.Requests(r.URL.Query())) })
	mux.HandleFunc("GET /api/ui/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, ok := b.Detail(r.PathValue("id"))
		if !ok {
			Error(w, 404, "Запрос уже удалён или не найден")
			return
		}
		JSON(w, 200, d)
	})
	mux.HandleFunc("POST /api/ui/actions", func(w http.ResponseWriter, r *http.Request) {
		if !b.Writable() {
			Error(w, 503, "Роутер сейчас не активен; настройки не изменены")
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			Error(w, 415, "Ожидается application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var action Action
		if err := decoder.Decode(&action); err != nil {
			Error(w, 400, "Некорректный или слишком большой JSON")
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF || action.Action == "" || action.Fields == nil {
			Error(w, 400, "Укажите действие и поля; допустим один JSON-объект")
			return
		}
		result, err := b.Action(r.Context(), action)
		if err != nil {
			Error(w, 400, err.Error())
			return
		}
		JSON(w, 200, result)
	})
	mux.HandleFunc("GET /api/ui/events", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			Error(w, 500, "Обновления недоступны")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			if _, err := fmt.Fprint(w, "event: refresh\ndata: {}\n\n"); err != nil {
				return
			}
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
		}
	})
	// Vite may emit non-bundled public assets at the root.
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) { serve.ServeHTTP(w, r) })
}

// ValidField rejects accidental unsupported action arguments at the boundary.
func ValidField(name, allowed string) bool { return strings.Contains(" "+allowed+" ", " "+name+" ") }
