#!/usr/bin/env bash
set -euo pipefail

# Node-3 single-node bootstrap for the local 1-sequencer + 3-endorser lab.
# Run this on node-3.

NODE_NAME="${NODE_NAME:-node-3}"
NODE_DATA_DIR="${NODE_DATA_DIR:-/data/nitro-data/node-3}"
LOG_DIR="${LOG_DIR:-/data/nitro-logs}"
MACHINES_DIR="${MACHINES_DIR:-/data/endorsement/machines}"
NITRO_VAL_BIN="${NITRO_VAL_BIN:-/data/endorsement/bin/nitro-val}"
ENDORSER_BIN="${ENDORSER_BIN:-/data/endorsement/bin/endorser}"
JWT_SECRET="${JWT_SECRET:-/data/nitro-data/val.jwt}"
BLS_SECRET_FILE="${BLS_SECRET_FILE:-/data/nitro-data/bls.hex}"
VAL_PORT="${VAL_PORT:-52001}"
ENDORSER_PORT="${ENDORSER_PORT:-9002}"
ENDORSER_ID="${ENDORSER_ID:-B}"
REJECT_TO="${REJECT_TO:-0x1111111111111111111111111111111111111111}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing command: $1" >&2
    exit 1
  }
}

need_cmd curl
need_cmd cat
need_cmd ss

mkdir -p "$NODE_DATA_DIR" "$LOG_DIR"

if [[ ! -f "$JWT_SECRET" ]]; then
  echo "missing JWT secret: $JWT_SECRET" >&2
  exit 1
fi

if [[ ! -f "$BLS_SECRET_FILE" ]]; then
  echo "missing BLS secret key file: $BLS_SECRET_FILE" >&2
  exit 1
fi

pkill -f "$NITRO_VAL_BIN" >/dev/null 2>&1 || true
pkill -f "$ENDORSER_BIN" >/dev/null 2>&1 || true

echo "[*] starting nitro-val on $NODE_NAME:$VAL_PORT"
nohup "$NITRO_VAL_BIN" \
  --file-logging.enable=false \
  --persistent.global-config "$NODE_DATA_DIR" \
  --validation.wasm.root-path "$MACHINES_DIR" \
  --validation.wasm.allowed-wasm-module-roots "$MACHINES_DIR" \
  --auth.addr 0.0.0.0 \
  --auth.port "$VAL_PORT" \
  --auth.origins '*' \
  --auth.jwtsecret "$JWT_SECRET" \
  > "$LOG_DIR/nitro-val.log" 2>&1 &

echo "[*] starting endorser $ENDORSER_ID on $NODE_NAME:$ENDORSER_PORT"
nohup "$ENDORSER_BIN" \
  -id "$ENDORSER_ID" \
  -listen "0.0.0.0:$ENDORSER_PORT" \
  -bls-secret-key "$(cat "$BLS_SECRET_FILE")" \
  -reject-to "$REJECT_TO" \
  > "$LOG_DIR/endorser.log" 2>&1 &

echo "[*] waiting for services"
for _ in $(seq 1 30); do
  if ss -lnt | grep -q ":$VAL_PORT " && curl -fsS "http://127.0.0.1:$ENDORSER_PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! ss -lnt | grep -q ":$VAL_PORT "; then
  echo "timeout waiting for nitro-val port $VAL_PORT" >&2
  exit 1
fi

if ! curl -fsS "http://127.0.0.1:$ENDORSER_PORT/healthz" >/dev/null 2>&1; then
  echo "timeout waiting for endorser health on $ENDORSER_PORT" >&2
  exit 1
fi

echo "[*] endorser health:"
curl -fsS "http://127.0.0.1:$ENDORSER_PORT/healthz"
echo
echo "[*] endorser pubkey:"
curl -fsS "http://127.0.0.1:$ENDORSER_PORT/pubkey"
echo
echo "[*] node-3 bootstrap finished"
