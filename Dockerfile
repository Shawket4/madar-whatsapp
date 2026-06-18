# ── Stage 1: build ────────────────────────────────────────────────────────────
# CGO required for mattn/go-sqlite3
FROM golang:1.25-alpine AS builder
WORKDIR /app
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o sufrix-whatsapp .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/sufrix-whatsapp .
EXPOSE 8090
CMD ["./sufrix-whatsapp"]
