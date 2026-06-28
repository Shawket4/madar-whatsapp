# madar-whatsapp

A small **multi-client WhatsApp send gateway** for local testing of the Madar
delivery **OTP** + status messages. Built on [whatsmeow](https://github.com/tulir/whatsmeow)
(the WhatsApp multi-device protocol in Go).

It speaks the exact contract the Rust backend calls
(`MadarRust/src/delivery/whatsapp.rs`):

```
POST {WHATSAPP_SERVICE_URL}/send/message
Body: { "phone": "201001234567", "message": "..." }
Optional header: Authorization: <WHATSAPP_AUTH_HEADER>
```

You can link **multiple** WhatsApp numbers ("sessions"); a send picks one via the
optional `"session"` body field, the `WA_DEFAULT_SESSION` env, or — if only one
number is linked — that one automatically. Sessions persist across restarts
(stored in a local SQLite file).

> Sender-only, fire-and-forget. No inbound/two-way. For **local testing** — it has
> no rate limiting or TLS; keep it on `localhost` (or behind nginx) if exposed.

---

## Build & run

```bash
cd ~/Desktop/madar-whatsapp
go build -o madar-whatsapp .
./madar-whatsapp            # listens on :8090 by default
# or: ./run.sh
```

### Configuration (env)

| Var | Default | Purpose |
|-----|---------|---------|
| `WA_ADDR` | `:8090` | listen address |
| `WA_STORE` | `whatsapp-store.db` | SQLite session store (whatsmeow) |
| `WA_META` | `whatsapp-sessions.json` | name ↔ JID sidecar |
| `WA_DEFAULT_SESSION` | _(empty)_ | session used when a send omits `session` |
| `WA_AUTH_TOKEN` | _(empty)_ | if set, `/send/message` requires `Authorization: <token>` (must match the backend's `WHATSAPP_AUTH_HEADER`) |
| `WA_LOG_LEVEL` | `INFO` | `DEBUG`/`INFO`/`WARN`/`ERROR` |

---

## Link a WhatsApp number (pairing)

**Easiest — browser:** open `http://localhost:8090/pair/main` and scan the QR with
your phone (WhatsApp → **Linked devices** → **Link a device**). The page shows
“✓ Linked!” when done. (`main` is just the session name — use any name.)

**Or — terminal:** `curl -X POST http://localhost:8090/sessions/main/pair` then scan
the QR printed in the server terminal.

Linked numbers survive restarts. Check status any time:

```bash
curl -s http://localhost:8090/sessions | jq
```

---

## End-to-end: test the delivery OTP

1. **Run this gateway** and link a number (above). For a self-contained test you
   can link your own phone and send the OTP **to that same number** (WhatsApp lets
   you message yourself).

2. **Point the backend at it.** In `MadarRust/.env`:
   ```
   WHATSAPP_SERVICE_URL=http://localhost:8090
   MADAR_OTP_DEBUG=1        # also returns the code in the HTTP response (dev only)
   # WHATSAPP_AUTH_HEADER=secret123   # only if you set WA_AUTH_TOKEN=secret123 here
   ```
   Then restart the backend (`cargo run` in `MadarRust`).

3. **Request an OTP** (the public delivery endpoint; rate-limited ~1/30s per IP):
   ```bash
   curl -s -X POST http://localhost:8081/public/otp/request \
     -H 'Content-Type: application/json' \
     -d '{"phone":"01001234567"}'
   # → {"sent":true,"debug_code":"1234"}   (debug_code only with MADAR_OTP_DEBUG=1)
   ```
   A WhatsApp message — *“Your Madar verification code is 1234. It expires in 5
   minutes.”* — arrives on that number. The same code flows through the normal
   delivery flow (order page → OTP → `POST /public/otp/verify`).

   You can also hit the gateway directly:
   ```bash
   curl -s -X POST http://localhost:8090/send/message \
     -H 'Content-Type: application/json' \
     -d '{"phone":"201001234567","message":"hello from madar-whatsapp"}'
   ```

---

## HTTP API

| Method & path | Purpose |
|---|---|
| `GET /health` | liveness |
| `GET /sessions` | list sessions + status |
| `POST /sessions/{name}/pair` | start pairing (returns the QR string + pair URL) |
| `GET  /pair/{name}` | browser pairing page (QR image + live status) |
| `GET  /sessions/{name}/qr.png` | current pairing QR as PNG |
| `GET  /sessions/{name}/status` | `{connected, logged_in, has_qr}` |
| `POST /sessions/{name}/logout` | unlink the number |
| `POST /send/message` | **the backend contract** — `{phone, message, session?}` |

`/send/message` returns `200 {"sent":true,"id":"..."}` on success, `503` if no
session is linked/ready, `400` for bad input, `502` if WhatsApp rejects the send.
