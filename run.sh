#!/usr/bin/env bash
# Build (if needed) and run the Sufrix WhatsApp gateway.
set -euo pipefail
cd "$(dirname "$0")"
go build -o sufrix-whatsapp .
exec ./sufrix-whatsapp "$@"
