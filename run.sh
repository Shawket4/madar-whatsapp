#!/usr/bin/env bash
# Build (if needed) and run the Madar WhatsApp gateway.
set -euo pipefail
cd "$(dirname "$0")"
go build -o madar-whatsapp .
exec ./madar-whatsapp "$@"
