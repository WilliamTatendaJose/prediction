# IoT Hub

A reusable sensor API and live dashboard in one ~7 MB Go binary:

- **Embedded MQTT broker** (port 1883), so devices need nothing else.
- **REST API** for devices that speak HTTP, and for managing sensors and the dashboard.
- **Server-Sent Events** for live updates to the browser.
- **Dashboard** with no build step: plain JS modules embedded in the binary. Tiles are plugins.

Adding a sensor takes no setup: publish data and it appears. Adding a tile type means writing one JS function. The backend doesn't change.

```
 devices ──MQTT iot/{sensor}[/{field}]──┐
                                        ├─► ingest.Pipeline ─► store (ring buffers) ─► stream.Hub ─SSE─► dashboard
 devices ──HTTP POST /api/sensors/…/data┘         │                                                      ▲
                                                  └─► (REST readings re-published to MQTT)    GET /api/… ┘
```

## Run

```bash
cd iot-hub
go run ./cmd/iothub                       # http :8080, mqtt :1883
go run ./cmd/simulate -every 1s           # demo data over MQTT + REST (second terminal)
open http://localhost:8080                # Edit → Add tile
```

Docker: `docker build -t iothub . && docker run -p 8080:8080 -p 1883:1883 -v iothub:/data iothub`

### Configuration (flag / env)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-http` | `IOTHUB_HTTP` | `:8080` | HTTP address |
| `-mqtt` | `IOTHUB_MQTT` | `:1883` | MQTT TCP address, empty disables |
| `-mqtt-ws` | `IOTHUB_MQTT_WS` | — | MQTT over WebSocket address |
| `-prefix` | `IOTHUB_PREFIX` | `iot` | MQTT topic prefix |
| `-data` | `IOTHUB_DATA` | `data/iothub.json` | sensor definitions and dashboard file |
| `-points` | `IOTHUB_POINTS` | `1024` | readings kept per sensor field |
| `-max-sensors` | `IOTHUB_MAX_SENSORS` | `500` | hard cap |
| `-max-fields` | `IOTHUB_MAX_FIELDS` | `16` | hard cap per sensor |
| `-auto-register` | `IOTHUB_AUTO_REGISTER` | `true` | create sensors and fields on first data |
| `-token` | `IOTHUB_TOKEN` | — | shared secret: HTTP `Authorization: Bearer …` for writes, MQTT password |

## Sending data

The payload rules are the same for both transports:

| Payload | Result |
|---|---|
| `{"temperature":21.5,"door":"Open","locked":true}` | three fields |
| `21.5` to `iot/env-1/temperature` or `POST …/data/temperature` | one field |
| `21.5` to `iot/env-1` | field `value` |
| `{"ts":1700000000, ...}` or `"timestamp":"2024-01-01T00:00:00Z"` | sample time (unix s, unix ms or RFC 3339). Defaults to now; >24 h in the future is ignored |

- Numbers and booleans become time series (booleans are stored as 0/1).
- Strings are kept as latest state only, e.g. `door: "Open"`.
- Nested objects and arrays are ignored.

```bash
mosquitto_pub -t iot/env-1 -m '{"temperature":21.5,"humidity":48}'
curl -X POST localhost:8080/api/sensors/env-1/data -d '{"temperature":21.5}'
```

Every reading, whatever transport it came in on, is also published on `iot/{sensor}`. Other services can subscribe there, for example the ML.NET prediction server in this repo. The existing `AccessControlEmulator` payload works unchanged if you point it at `POST /api/sensors/access-1/data`.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/sensors` | all sensors with `last` values and `lastSeen` |
| GET / PUT / DELETE | `/api/sensors/{id}` | definition: `name`, `kind`, `location`, `fields{name:{label,unit,min,max}}` |
| POST | `/api/sensors/{id}/data[/{field}]` | ingest, returns 202 |
| GET | `/api/sensors/{id}/history?field=&limit=&since=` | columnar `{t:[ms…], v:[…]}` |
| GET / PUT | `/api/dashboard` | layout JSON (opaque to the server) |
| GET | `/api/stream[?sensors=a,b]` | SSE, `data: {"s":id,"t":ms,"v":{field:value}}` |
| GET | `/api/health` | heap, stream clients, dropped messages |

IDs and field names must match `[A-Za-z0-9_.-]{1,64}`.

## Adding a tile type

Tiles live in `web/tiles.js`. Register one, and it appears in the "Add tile" dialog:

```js
registerTile('big-number', {
  label: 'Big number',
  options: [{ key: 'decimals', label: 'Decimals', type: 'number' }],
  create(el, cfg, ctx) {            // ctx: sensor, unit(f), history(f, n), invalidate()
    const d = document.createElement('div'); d.className = 'value'; el.append(d);
    let v;
    return {
      update(t, value) { v = value; },                 // record only; called per reading
      render() { d.textContent = v?.toFixed(cfg.options?.decimals ?? 0); }, // ≤ once per frame
    };
  },
});
```

The built-in tiles are `stat` (value, status and sparkline), `meter` (range bar with thresholds), `line` (up to 4 fields with a crosshair tooltip) and `state` (text or on/off with a "normal" value). For thresholds, set `warn` and `crit`; if `crit < warn`, low values are treated as bad. Fields in one `line` tile share a y-axis, so only group fields on the same scale.

## Efficiency, by design

| Choice | Effect |
|---|---|
| Fixed ring buffer per field (`int64` ts + `float32` value) | 12 B/point, allocated once, never grows. Worst case = sensors × fields × points × 12 B (default caps: 94 MB; real use is far lower because rings are allocated only for fields that report) |
| One JSON encode per reading, shared by all SSE clients | fan-out cost doesn't grow with message size |
| Slow SSE clients drop messages instead of blocking | ingestion never stalls; drops show in `/api/health` |
| SSE bursts flushed together | one syscall per burst, not per reading |
| Columnar history `{t:[],v:[]}` | smaller payload, maps straight to typed arrays |
| Browser: typed-array rings, render ≤ 1×/frame, canvas charts, no framework | page does constant work at high message rates |
| Hidden tab closes its stream | zero cost when nobody is looking |
| Request body caps (16 KB ingest, 256 KB config), MQTT packet cap 64 KB | bounded memory per request |

**Measured** in this container: 204 sensors at ~5 Hz each (~1,000 msgs/s over MQTT) used **2.2% of one CPU core** and **20 MB RSS** (6.7 MB heap). With 4 sensors, RSS was 12 MB.

## Limits and next steps

- **Readings are in memory only.** They're lost on restart; definitions and the dashboard persist. For long history, add a sink in `ingest.Pipeline` (SQLite, or TimescaleDB/InfluxDB) without touching the transports.
- **Auth is a single shared token.** Reads (dashboard, stream) are open. Put it behind a reverse proxy with TLS, or add per-device credentials, before exposing it beyond a LAN.
- **The embedded broker is a single node.** To use an existing broker (Mosquitto, EMQX), run with `-mqtt ""` and add a small subscriber that calls `Pipeline.Handle`.
- **Alerts:** thresholds currently only colour tiles. A server-side rule that runs in the pipeline would be the next step.
