#!/usr/bin/env bash
set -euo pipefail

# Node-1 one-click bootstrap for the local 1-sequencer + 3-endorser lab.
# Run this on node-1 after node-2/node-3/node-4 are already up.

RPC_URL="${RPC_URL:-http://127.0.0.1:8547}"
CHAIN_DIR="${CHAIN_DIR:-/data/nitro-data/node-1/chain}"
NODE1_DATA_DIR="${NODE1_DATA_DIR:-/data/nitro-data/node-1}"
LOG_DIR="${LOG_DIR:-/data/nitro-logs}"
MACHINES_DIR="${MACHINES_DIR:-/data/endorsement/machines}"
NITRO_BIN="${NITRO_BIN:-/data/endorsement/bin/nitro}"

PEER_A_RPC="${PEER_A_RPC:-http://192.168.1.13:9001}"
PEER_B_RPC="${PEER_B_RPC:-http://192.168.1.6:9002}"
PEER_C_RPC="${PEER_C_RPC:-http://192.168.1.4:9003}"

PEER_A_VAL="${PEER_A_VAL:-ws://192.168.1.13:52000}"
PEER_B_VAL="${PEER_B_VAL:-ws://192.168.1.6:52001}"
PEER_C_VAL="${PEER_C_VAL:-ws://192.168.1.4:52002}"

FUNDER_KEY="${FUNDER_KEY:-0xb6b15c8cb491557369f3c7d2c287b053eb229daa9c22138887752191c9520659}"
FUNDER_KEY_NOX="${FUNDER_KEY#0x}"
CHAIN_ID="${CHAIN_ID:-421613}"
BATCHING_WINDOW_MS="${BATCHING_WINDOW_MS:-800}"
ENDORSEMENT_MODE="${ENDORSEMENT_MODE:-remote}"
DEFAULT_THRESHOLD="${DEFAULT_THRESHOLD:-2}"
STRICT_THRESHOLD="${STRICT_THRESHOLD:-3}"
DEFAULT_AGGREGATION="${DEFAULT_AGGREGATION:-bls}"
STRICT_AGGREGATION="${STRICT_AGGREGATION:-bls}"
BLOCK_ENDORSEMENT_TIMEOUT_MS="${BLOCK_ENDORSEMENT_TIMEOUT_MS:-2000}"
MAX_REBUILD_ROUNDS="${MAX_REBUILD_ROUNDS:-3}"

reset_chain="${RESET_CHAIN:-1}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing command: $1" >&2
    exit 1
  }
}

need_cmd curl
need_cmd jq
need_cmd cast

mkdir -p "$NODE1_DATA_DIR" "$LOG_DIR"

if [[ "$reset_chain" == "1" ]]; then
  pkill -f "$NITRO_BIN" >/dev/null 2>&1 || true
  rm -rf "$CHAIN_DIR"
  mkdir -p "$CHAIN_DIR"
fi

fetch_json_field() {
  local url="$1"
  local field="$2"
  curl -fsS --connect-timeout 5 --max-time 10 "$url" | jq -r "$field"
}

wait_for_ok() {
  local url="$1"
  local label="$2"
  for _ in $(seq 1 60); do
    if curl -fsS --connect-timeout 3 --max-time 5 "$url" >/dev/null 2>&1; then
      return 0
    fi
    echo "waiting for $label..."
    sleep 2
  done
  echo "timeout waiting for $label" >&2
  exit 1
}

echo "[*] waiting for endorsers to answer /healthz"
wait_for_ok "${PEER_A_RPC%/}/healthz" "endorser A"
wait_for_ok "${PEER_B_RPC%/}/healthz" "endorser B"
wait_for_ok "${PEER_C_RPC%/}/healthz" "endorser C"

echo "[*] fetching BLS public keys"
PUBKEY_A="$(fetch_json_field "${PEER_A_RPC%/}/pubkey" '.public_key_hex')"
PUBKEY_B="$(fetch_json_field "${PEER_B_RPC%/}/pubkey" '.public_key_hex')"
PUBKEY_C="$(fetch_json_field "${PEER_C_RPC%/}/pubkey" '.public_key_hex')"

if [[ -z "$PUBKEY_A" || -z "$PUBKEY_B" || -z "$PUBKEY_C" ]]; then
  echo "failed to fetch one or more pubkeys" >&2
  exit 1
fi

echo "[*] using pubkey A: $PUBKEY_A"
echo "[*] using pubkey B: $PUBKEY_B"
echo "[*] using pubkey C: $PUBKEY_C"

echo "[*] starting node-1 nitro"
nohup "$NITRO_BIN" \
  --file-logging.enable=false \
  --persistent.global-config "$NODE1_DATA_DIR" \
  --persistent.chain "$CHAIN_DIR" \
  --init.dev-init \
  --init.empty=false \
  --node.dangerous.no-l1-listener=true \
  --node.dangerous.no-sequencer-coordinator \
  --node.staker.enable=false \
  --chain.dev-wallet.private-key="$FUNDER_KEY_NOX" \
  --chain.id "$CHAIN_ID" \
  --http.addr 0.0.0.0 \
  --http.port 8547 \
  --http.vhosts '*' \
  --http.corsdomain '*' \
  --ws.addr 0.0.0.0 \
  --ws.port 8548 \
  --ws.origins '*' \
  --execution.sequencer.experimental-batching-window="${BATCHING_WINDOW_MS}ms" \
  --validation.wasm.allowed-wasm-module-roots "$MACHINES_DIR" \
  --node.sequencer \
  --execution.sequencer.enable \
  --node.feed.input.url= \
  --node.feed.input.secondary-url= \
  --node.feed.output.enable \
  --node.feed.output.port 9642 \
  --node.block-validator.enable \
  --node.block-validator.validation-server-configs-list="[\
    {\"url\":\"$PEER_A_VAL\",\"jwtsecret\":\"/data/nitro-data/val.jwt\"},\
    {\"url\":\"$PEER_B_VAL\",\"jwtsecret\":\"/data/nitro-data/val.jwt\"},\
    {\"url\":\"$PEER_C_VAL\",\"jwtsecret\":\"/data/nitro-data/val.jwt\"}\
  ]" \
  --execution.endorsement-experiment.enable=true \
  --execution.endorsement-experiment.mode="$ENDORSEMENT_MODE" \
  --execution.endorsement-experiment.default-threshold="$DEFAULT_THRESHOLD" \
  --execution.endorsement-experiment.strict-threshold="$STRICT_THRESHOLD" \
  --execution.endorsement-experiment.default-aggregation="$DEFAULT_AGGREGATION" \
  --execution.endorsement-experiment.strict-aggregation="$STRICT_AGGREGATION" \
  --execution.endorsement-experiment.block-endorsement-timeout="${BLOCK_ENDORSEMENT_TIMEOUT_MS}ms" \
  --execution.endorsement-experiment.max-rebuild-rounds="$MAX_REBUILD_ROUNDS" \
  --execution.endorsement-experiment.fail-to-address=0x1111111111111111111111111111111111111111 \
  --execution.endorsement-experiment.endorser-a-url="$PEER_A_RPC" \
  --execution.endorsement-experiment.endorser-b-url="$PEER_B_RPC" \
  --execution.endorsement-experiment.endorser-c-url="$PEER_C_RPC" \
  --execution.endorsement-experiment.endorser-a-pubkey="$PUBKEY_A" \
  --execution.endorsement-experiment.endorser-b-pubkey="$PUBKEY_B" \
  --execution.endorsement-experiment.endorser-c-pubkey="$PUBKEY_C" \
  --execution.forwarding-target null \
  > "$LOG_DIR/nitro.log" 2>&1 &

echo "[*] waiting for rpc"
for _ in $(seq 1 60); do
  if cast block-number --rpc-url "$RPC_URL" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

echo "[*] current chain id: $(cast chain-id --rpc-url "$RPC_URL")"
echo "[*] current block number: $(cast block-number --rpc-url "$RPC_URL")"
echo "[*] funder address: $(cast wallet address --private-key "$FUNDER_KEY")"
echo "[*] funder balance: $(cast balance "$(cast wallet address --private-key "$FUNDER_KEY")" --rpc-url "$RPC_URL")"
echo "[*] node-1 bootstrap finished"
