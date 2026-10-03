#!/usr/bin/env bash
# Runs a demo hub with simulated sensors and devices that answer commands.
#   ./scripts/demo.sh        then open http://localhost:8080
# Needs Go and Python 3. Stop with Ctrl+C. Data goes to ./demo-data (deleted on start).
set -euo pipefail
cd "$(dirname "$0")/.."
D=demo-data
rm -rf "$D" && mkdir -p "$D/bin"
for c in iothub simulate devicesim; do go build -o "$D/bin/$c" ./cmd/$c; done
pids=()
trap 'kill "${pids[@]}" 2>/dev/null' EXIT
# -auth-file too: it defaults to ./data/devices.json, so without it the demo
# leaves credentials outside demo-data and a re-run fails with 409 Conflict.
"$D/bin/iothub" -tenancy multi -token platform-demo-token -data "$D/iothub.json" -db "sqlite:$D/readings.db" \
  -auth-file "$D/devices.json" -backup-dir "$D/backups" -http 127.0.0.1:8080 -mqtt 127.0.0.1:1883 > "$D/hub.log" 2>&1 & pids+=($!)
for _ in $(seq 50); do curl -sf 127.0.0.1:8080/api/health >/dev/null && break; sleep 0.2; done
python3 scripts/demo_seed.py "$D/creds.json"
cred() { python3 -c "import json,sys;print(json.load(open('$D/creds.json'))[sys.argv[1]])" "$1"; }
"$D/bin/simulate" -mqtt "" -http http://127.0.0.1:8080 -token "$(cred gateway-1)" -every 1s > "$D/sim.log" 2>&1 & pids+=($!)
for d in pump-1 pump-2 valve-1; do "$D/bin/devicesim" -cs "$(cred $d)" -mqtt-port 1883 > "$D/$d.log" 2>&1 & pids+=($!); done
curl -s -XPOST 127.0.0.1:8080/api/sensors/meter-9/data -H "Authorization: Bearer $(cred meter-9)" -d '{"kwh":1204.5}' >/dev/null
cat <<MSG

  IoT Hub demo is running: http://localhost:8080

  Sign in with one of these tokens:
    Platform operator   platform-demo-token
    Admin (Acme)        $(cred ops)
    Operator (Acme)     $(cred shift)
    Viewer (Acme)       $(cred screen)

  pump-1, pump-2 and valve-1 answer commands; pump-3 stays offline (watch retries);
  meter-9 goes overdue after ~2 minutes. Ctrl+C stops everything.
MSG
wait
