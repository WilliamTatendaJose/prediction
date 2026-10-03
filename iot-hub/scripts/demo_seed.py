# Seeds the demo used by scripts/demo.sh: two tenants, people for each role,
# devices, sensor limits, commands with retries and a dashboard.
import json, urllib.request, sys
U = "http://127.0.0.1:8080"; SUP = "platform-demo-token"
def call(method, path, body=None, tok=SUP, tenant="acme"):
    req = urllib.request.Request(U + path, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Bearer " + tok)
    if tenant and not path.startswith("/api/admin"): req.add_header("X-Tenant", tenant)
    try:
        with urllib.request.urlopen(req) as r:
            t = r.read(); return json.loads(t) if t else None
    except urllib.error.HTTPError as e:
        sys.exit(f"{method} {path}: {e.code} {e.read().decode()}")
out = {}
call("POST", "/api/admin/tenants", {"id": "acme", "name": "Acme Mining", "quota": {"messagesPerDay": 500000, "maxSensors": 200, "maxDevices": 100}})
call("POST", "/api/admin/tenants", {"id": "globex", "name": "Globex Foods"})
for who, role in [("ops", "admin"), ("shift", "operator"), ("screen", "viewer")]:
    out[who] = call("POST", "/api/devices", {"id": who, "role": role, "auth": "token", "note": {"ops": "Plant manager", "shift": "Shift supervisor", "screen": "Control-room screen"}[who]})["token"]
out["gateway-1"] = call("POST", "/api/devices", {"id": "gateway-1", "role": "service", "sensors": ["*"], "auth": "token", "note": "Edge gateway (simulator)"})["token"]
for d, pat, note in [("pump-1", "pump-1", "Borehole pump A"), ("pump-2", "pump-2", "Borehole pump B"), ("pump-3", "pump-3", "Spare pump (offline)"), ("valve-1", "valve-1", "Main supply valve")]:
    out[d] = call("POST", "/api/devices", {"id": d, "role": "device", "sensors": [pat], "auth": "keys", "note": note})["connectionString"]
out["meter-9"] = call("POST", "/api/devices", {"id": "meter-9", "role": "device", "sensors": ["meter-9"], "auth": "token", "note": "Energy meter, substation"})["token"]
call("PUT", "/api/sensors/env-1", {"name": "Boiler room", "kind": "environment", "location": "Plant A", "fields": {"temperature": {"unit": "°C", "min": 0, "max": 50, "detect": {"high": 24}}, "humidity": {"unit": "%", "min": 0, "max": 100}}})
call("PUT", "/api/sensors/tank-1", {"name": "Raw water tank", "kind": "tank", "location": "Plant A", "fields": {"level": {"unit": "%", "min": 0, "max": 100, "detect": {"low": 10, "high": 95}}}})
call("PUT", "/api/sensors/power-1", {"name": "Main feeder", "kind": "meter", "fields": {"voltage": {"unit": "V"}, "current": {"unit": "A"}, "power": {"unit": "W"}, "kw": {"label": "Power (kW)", "unit": "kW", "calc": {"formula": "voltage * current / 1000"}}}})
call("PUT", "/api/commands/reboot", {"label": "Reboot", "devices": ["pump-*"], "kind": "method", "timeout": "5s", "confirm": True,
    "params": [{"name": "delay", "label": "Delay (s)", "type": "number", "min": 0, "max": 60, "default": 5}],
    "retry": {"attempts": 3, "every": "15s", "backoff": 2, "jitter": 0.2}})
call("PUT", "/api/commands/set-valve", {"label": "Set valve", "devices": ["valve-*", "pump-*"], "kind": "message", "payload": {"cmd": "valve"},
    "params": [{"name": "position", "label": "Position", "type": "choice", "choices": ["open", "closed"], "required": True}]})
call("PUT", "/api/devices/meter-9/expected-interval", {"interval": "1m"})
call("PUT", "/api/dashboard", {"tiles": [
    {"id": "t1", "type": "stat", "sensor": "tank-1", "field": "level", "w": 1, "h": 1, "options": {"warn": 80, "crit": 95}},
    {"id": "t2", "type": "meter", "sensor": "env-1", "field": "temperature", "w": 1, "h": 1, "options": {"min": 0, "max": 50, "warn": 22, "crit": 24}},
    {"id": "t3", "type": "line", "sensor": "power-1", "fields": ["kw"], "w": 2, "h": 2, "options": {}},
    {"id": "t4", "type": "anomalies", "sensor": "", "w": 2, "h": 2, "options": {}},
    {"id": "t5", "type": "state", "sensor": "door-1", "field": "door", "w": 1, "h": 1, "options": {"ok": "closed"}},
    {"id": "t6", "type": "stat", "sensor": "pump-1", "field": "vibration", "w": 1, "h": 1, "options": {}}]})
json.dump(out, open(sys.argv[1], "w"), indent=1)
print("seeded:", ", ".join(out))
