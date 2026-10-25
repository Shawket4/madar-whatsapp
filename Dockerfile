# ── Stage 1: build ────────────────────────────────────────────────────────────
# CGO required for mattn/go-sqlite3
FROM golang:1.26-alpine AS builder
WORKDIR /app
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GOAMD64 / CPU_MARCH: CI deploys pass v4 / x86-64-v4 (AVX-512) for the VPS's
# AMD EPYC; the defaults are the baseline, so a local image runs anywhere.
# CGO_CFLAGS covers the SQLite C code, which GOAMD64 does not touch.
ARG GOAMD64=v1
ARG CPU_MARCH=x86-64
RUN CGO_ENABLED=1 GOOS=linux GOAMD64=${GOAMD64} CGO_CFLAGS="-O3 -march=${CPU_MARCH}" \
    go build -trimpath -ldflags="-w -s" -o madar-whatsapp .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/madar-whatsapp .
EXPOSE 8090
CMD ["./madar-whatsapp"]
