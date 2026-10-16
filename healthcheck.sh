#!/bin/sh
set -eu

addr="${ADDR:-:8080}"
port="${addr##*:}"
host="${addr%:*}"
case "$host" in
	"" | "0.0.0.0" | "[::]" | "*") host="127.0.0.1" ;;
esac

exec curl -f "http://$host:${port:-8080}/readyz"
