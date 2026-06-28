// madar-whatsapp — a small multi-client WhatsApp send gateway for local testing
// of the Madar delivery OTP + status messages.
//
// It speaks the exact contract the Rust backend calls (see MadarRust
// src/delivery/whatsapp.rs): POST /send/message {phone, message} with an optional
// Authorization header. Multiple WhatsApp numbers can be linked ("sessions");
// /send/message picks one via the optional "session" field, the WA_DEFAULT_SESSION
// env, or the single logged-in session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type config struct {
	addr           string
	storePath      string
	metaPath       string
	defaultSession string
	authToken      string
	logLevel       string
}

func loadConfig() config {
	return config{
		addr:           env("WA_ADDR", ":8090"),
		storePath:      env("WA_STORE", "whatsapp-store.db"),
		metaPath:       env("WA_META", "whatsapp-sessions.json"),
		defaultSession: env("WA_DEFAULT_SESSION", ""),
		authToken:      env("WA_AUTH_TOKEN", ""),
		logLevel:       env("WA_LOG_LEVEL", "INFO"),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig()
	logger := waLog.Stdout("wa", cfg.logLevel, true)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr, err := NewManager(ctx, cfg.storePath, cfg.metaPath, cfg.defaultSession, logger)
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           routes(mgr, cfg.authToken),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Infof("listening on %s (store=%s, default_session=%q, auth=%v)",
			cfg.addr, cfg.storePath, cfg.defaultSession, cfg.authToken != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Infof("shutting down…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	mgr.DisconnectAll()
}

func routes(mgr *Manager, authToken string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": mgr.List()})
	})

	mux.HandleFunc("POST /sessions/{name}/pair", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		c, err := mgr.Pair(r.Context(), name)
		if err != nil && !errors.Is(err, ErrSessionExists) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if errors.Is(err, ErrSessionExists) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "session already linked", "name": name})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":     name,
			"qr":       c.QR(),
			"pair_url": "/pair/" + name,
			"message":  "Open the pair URL (or scan the QR printed in the server terminal) with WhatsApp → Linked devices.",
		})
	})

	mux.HandleFunc("GET /sessions/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		c, ok := mgr.Get(name)
		if !ok {
			writeJSON(w, http.StatusOK, SessionInfo{Name: name})
			return
		}
		jid := ""
		if c.WA.Store.ID != nil {
			jid = c.WA.Store.ID.String()
		}
		writeJSON(w, http.StatusOK, SessionInfo{
			Name:      name,
			JID:       jid,
			Connected: c.WA.IsConnected(),
			LoggedIn:  c.WA.IsLoggedIn(),
			HasQR:     c.QR() != "",
		})
	})

	mux.HandleFunc("GET /sessions/{name}/qr.png", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		c, ok := mgr.Get(name)
		if !ok || c.QR() == "" {
			http.Error(w, "no active QR (already linked, or pairing not started)", http.StatusNotFound)
			return
		}
		png, err := qrcode.Encode(c.QR(), qrcode.Medium, 512)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
	})

	mux.HandleFunc("GET /pair/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		// Kick off pairing if needed (no-op if already logged in).
		if c, ok := mgr.Get(name); !ok || (!c.WA.IsLoggedIn() && c.QR() == "") {
			_, _ = mgr.Pair(r.Context(), name)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pairPage(name)))
	})

	mux.HandleFunc("POST /sessions/{name}/logout", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := mgr.Logout(r.Context(), name); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "logged_out": true})
	})

	// The endpoint the Rust backend calls. Optionally auth-gated.
	mux.HandleFunc("POST /send/message", requireAuth(authToken, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Phone   string `json:"phone"`
			Message string `json:"message"`
			Session string `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		if strings.TrimSpace(body.Phone) == "" || strings.TrimSpace(body.Message) == "" {
			writeErr(w, http.StatusBadRequest, errors.New("phone and message are required"))
			return
		}
		if body.Session == "" {
			body.Session = r.Header.Get("X-WA-Session")
		}
		id, err := mgr.Send(r.Context(), body.Session, body.Phone, body.Message)
		if err != nil {
			status := http.StatusBadGateway
			switch {
			case errors.Is(err, ErrUnknownSession), errors.Is(err, ErrAmbiguousSession):
				status = http.StatusServiceUnavailable
			case errors.Is(err, ErrNotLoggedIn):
				status = http.StatusServiceUnavailable
			}
			writeErr(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sent": true, "id": id})
	}))

	return logRequests(mux)
}

func requireAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != token {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next(w, r)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func pairPage(name string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>Link WhatsApp · ` + name + `</title>
<style>body{font-family:system-ui,sans-serif;background:#0a2540;color:#fff;display:grid;place-items:center;height:100vh;margin:0}
.card{background:#fff;color:#0a2540;border-radius:16px;padding:28px;text-align:center;box-shadow:0 12px 40px rgba(0,0,0,.3);max-width:360px}
h1{font-size:18px;margin:0 0 4px}p{color:#5b6b7b;font-size:13px;margin:6px 0 16px}
img{width:280px;height:280px;border-radius:8px}.ok{color:#1a8a4a;font-weight:700;font-size:16px}.muted{font-size:12px;color:#8a9bab}</style></head>
<body><div class="card">
<h1>Link WhatsApp — "` + name + `"</h1>
<p>WhatsApp → <b>Linked devices</b> → <b>Link a device</b>, then scan:</p>
<div id="box"><img id="qr" alt="QR loading…"></div>
<p class="muted" id="status">Waiting for QR…</p>
</div>
<script>
const name=` + jsString(name) + `;
async function tick(){
 try{
  const s=await (await fetch('/sessions/'+encodeURIComponent(name)+'/status')).json();
  if(s.logged_in){document.getElementById('box').innerHTML='<div class="ok">✓ Linked!</div>';document.getElementById('status').textContent='This number can now send messages.';return;}
  if(s.has_qr){document.getElementById('qr').src='/sessions/'+encodeURIComponent(name)+'/qr.png?'+Date.now();document.getElementById('status').textContent='Scan the QR with your phone.';}
  else{document.getElementById('status').textContent='Starting…';}
 }catch(e){document.getElementById('status').textContent='Error: '+e;}
 setTimeout(tick,2500);
}
tick();
</script></body></html>`
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
