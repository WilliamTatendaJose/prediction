# IoT Hub

A reusable sensor platform in one Go binary of ~15 MB (stripped):

- **Ingest** over an embedded **MQTT broker** (port 1883) or the **REST API**.
- **Persist** to **SQLite** (embedded, default) or **PostgreSQL/TimescaleDB**, with automatic 1-minute rollups and retention.
- **Analyse** with bucketed min/avg/max series and summary statistics over any time range.
- **Detect anomalies** as data streams in: limit breaches, spikes and silent sensors. Each anomaly reaches the dashboard, the database and MQTT.
- **Dashboard** with no build step: plain JS embedded in the binary. Tiles are plugins.

Adding a sensor takes no setup: publish data and it appears. Adding a tile type means writing one JS function. The backend doesn't change.

```
 devices ─MQTT iot/{sensor}[/{field}]─┐                         ┌─► ring buffers (live, bounded) ──┐
                                      ├─► ingest.Pipeline ──────┼─► anomaly.Detector ─ episodes ───┼─► SSE ─► dashboard
 devices ─HTTP POST …/data───────────┘                         └─► tsdb.Writer (batched) ─► SQLite │  MQTT iot-events/anomaly/…
                                                                                    or Postgres ─► analytics API ─┘
```

## Run

```bash
cd iot-hub
go run ./cmd/iothub                       # http :8080, mqtt :1883, SQLite at data/readings.db
go run ./cmd/simulate -every 1s           # demo data with injected faults (second terminal)
open http://localhost:8080                # Edit → Add tile
```

Postgres instead of SQLite: `go run ./cmd/iothub -db postgres://user:pass@host/iothub`. Tables are created on start.
Memory only (no database): `-db ""`.

Docker: `docker build -t iothub . && docker run -p 8080:8080 -p 1883:1883 -v iothub:/data iothub`

### Configuration (flag / env)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-http` | `IOTHUB_HTTP` | `:8080` | HTTP address |
| `-mqtt` | `IOTHUB_MQTT` | `:1883` | MQTT TCP address, empty disables |
| `-mqtt-ws` | `IOTHUB_MQTT_WS` | — | MQTT over WebSocket address |
| `-prefix` | `IOTHUB_PREFIX` | `iot` | MQTT topic prefix |
| `-data` | `IOTHUB_DATA` | `data/iothub.json` | sensor definitions and dashboard file |
| `-db` | `IOTHUB_DB` | `data/readings.db` | `file.db`, `sqlite:file`, `postgres://…`; empty = memory only |
| `-raw-retention` | `IOTHUB_RAW_RETENTION` | `168h` | raw readings kept (0 = forever) |
| `-rollup-retention` | `IOTHUB_ROLLUP_RETENTION` | `8760h` | 1-minute rollups kept (0 = forever) |
| `-points` | `IOTHUB_POINTS` | `1024` | live readings kept in memory per field |
| `-max-sensors` | `IOTHUB_MAX_SENSORS` | `500` | hard cap |
| `-max-fields` | `IOTHUB_MAX_FIELDS` | `16` | hard cap per sensor |
| `-auto-register` | `IOTHUB_AUTO_REGISTER` | `true` | create sensors and fields on first data |
| `-anomaly` | `IOTHUB_ANOMALY` | `true` | streaming anomaly detection |
| `-anomaly-z` | `IOTHUB_ANOMALY_Z` | `5` | spike threshold, standard deviations |
| `-anomaly-window` | `IOTHUB_ANOMALY_WINDOW` | `300` | EWMA span, samples |
| `-anomaly-persist` | `IOTHUB_ANOMALY_PERSIST` | `2` | consecutive outliers before a spike opens |
| `-anomaly-warmup` | `IOTHUB_ANOMALY_WARMUP` | `30` | samples before spike detection starts |
| `-stale-min` | `IOTHUB_STALE_MIN` | `1m` | minimum silence before "stale" (0 disables) |
| `-token` | `IOTHUB_TOKEN` | — | shared secret: HTTP `Authorization: Bearer …` for writes, MQTT password |

## Sending data

The payload rules are the same for both transports:

| Payload | Result |
|---|---|
| `{"temperature":21.5,"door":"Open","locked":true}` | three fields |
| `21.5` to `iot/env-1/temperature` or `POST …/data/temperature` | one field |
| `21.5` to `iot/env-1` | field `value` |
| `{"ts":1700000000, ...}` (also `timestamp`, `Timestamp`) | sample time (unix s, unix ms or RFC 3339). Defaults to now; >24 h in the future is ignored. Use it to backfill |

- Numbers and booleans (stored as 0/1) become time series, both in memory and in the database.
- Strings are state: the latest value is kept live, and each *change* is stored in the database.
- Nested objects and arrays are ignored.

```bash
mosquitto_pub -t iot/env-1 -m '{"temperature":21.5,"humidity":48}'
curl -X POST localhost:8080/api/sensors/env-1/data -d '{"temperature":21.5}'
```

Every reading, whatever transport it came in on, is also published on `iot/{sensor}`, and every anomaly on `iot-events/anomaly/{sensor}`. Other services can subscribe there, for example the ML.NET prediction server in this repo. The existing `AccessControlEmulator` payload works unchanged if you point it at `POST /api/sensors/access-1/data`.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/sensors` | all sensors with `last` values and `lastSeen` |
| GET / PUT / DELETE | `/api/sensors/{id}` | definition: `name`, `kind`, `location`, `fields{name:{label,unit,min,max,detect}}` |
| POST | `/api/sensors/{id}/data[/{field}]` | ingest, returns 202 |
| GET | `/api/sensors/{id}/history?field=&limit=&since=` | live ring buffer, columnar `{t, v}` |
| GET | `/api/sensors/{id}/series?field=&from=&to=&bucket=` | bucketed `{t, n, min, max, avg}`; `bucket` = `auto` (≤600 points), or a duration like `5m` |
| GET | `/api/sensors/{id}/stats?field=&from=&to=` | `{n, min, max, mean, std}` |
| GET | `/api/anomalies?sensor=&from=&to=&limit=&active=1` | anomaly episodes, newest first |
| GET / PUT | `/api/dashboard` | layout JSON (opaque to the server) |
| GET | `/api/stream[?sensors=a,b]` | SSE: readings as `data: {"s","t","v"}`; anomalies as `event: anomaly` |
| GET | `/api/health` | heap, stream and DB writer counters, active anomalies |

`from`/`to` accept unix ms, RFC 3339, `now`, or a duration before now: `-15m`, `-24h`, `-7d`. Defaults: the last hour (7 days for anomalies). Each analytics result includes `source` (`db` or `memory`), so you can tell whether it came from the database or the in-memory buffer.

```bash
curl 'localhost:8080/api/sensors/env-1/series?field=temperature&from=-24h&bucket=15m'
curl 'localhost:8080/api/sensors/env-1/stats?field=temperature&from=-7d'
curl 'localhost:8080/api/anomalies?active=1'
```

IDs and field names must match `[A-Za-z0-9_.-]{1,64}`.

## Database

| | SQLite (default) | PostgreSQL / TimescaleDB |
|---|---|---|
| Setup | none; a file next to the binary | `-db postgres://…` |
| Driver | `modernc.org/sqlite`: pure Go, so the binary stays static (no cgo) | `pgx` |
| Fits | an edge box or a single site; up to a few thousand writes/s | central storage, many sites, SQL/BI tools, HA |

Schema (identical on both):

- **`series`** maps `(sensor, field)` to a small integer id, so readings rows don't repeat strings.
- **`readings(series, ts, value, text)`** is the primary key `(series, ts)`, clustered (`WITHOUT ROWID` in SQLite). A range query is one sequential index scan.
- **`rollup_1m(series, minute, n, sum, sumsq, min, max)`** is updated in the same transaction as each batch. Every column is mergeable, so any bucket ≥ 1 minute and stats over windows > 6 h come from rollups instead of raw rows.
- **`anomalies`** holds one row per episode, closed in place (`end_ts`).

How writes work:

- Points go into a bounded queue (16k). One goroutine flushes it every second or every 500 points, as multi-row `INSERT`s plus the rollup upserts in a single transaction.
- If the database stalls, points are **dropped and counted** (`/api/health → db.dropped`) rather than blocking MQTT/HTTP ingestion.
- On shutdown the queue is drained after the transports close.

Retention runs hourly, per series, along the primary key. On TimescaleDB you can also convert `readings` to a hypertable; the queries don't change.

## Anomaly detection

Detection is streaming: O(1) work and about 100 bytes per field, with no history scans. Three detectors run on every numeric field:

| Kind | Fires when | Notes |
|---|---|---|
| `range` | value < `low` or > `high` | per field, set in the sensor definition |
| `spike` | \|value − EWMA mean\| > `z` × EWMA std for `persist` consecutive samples | learns each field's normal; no configuration needed |
| `stale` | no data for max(`stale-min`, 5 × the sensor's usual interval) | per sensor; learns the interval, so hourly and 5 Hz sensors both work |

- **Episodes, not samples.** Each anomaly is one event when it opens and the same event (same `id`, `end` set) when it closes. A sensor stuck out of range is one alert, not thousands.
- **Robust baseline.** Outliers update the baseline clipped to the threshold, so one glitch can't hide the next one. A genuine level shift becomes the new normal: the episode closes after ~20 samples for a 20σ jump and ~45 for a 100σ jump.
- **Hysteresis.** A spike closes below 0.8 × z, which avoids flapping.

Per-field rules:

```bash
curl -X PUT localhost:8080/api/sensors/tank-1 -d '{
  "fields": { "level": { "unit": "%", "detect": { "low": 15, "high": 98, "z": -1 } } } }'
#   low/high: range limits     z: spike threshold (-1 disables spikes)
#   persist: consecutive outliers (1 = catch single-sample glitches)     off: true disables all
```

**How the defaults were chosen.** False alarms per 100k samples, measured with `internal/anomaly`:

| z | window | persist | Gaussian noise | Heavy-tailed noise (Student-t, 5 dof) |
|---|---|---|---|---|
| 4 | 100 | 1 | 13.5 | 471 |
| 5 | 300 | 1 | 0 | 164 |
| **5** | **300** | **2 (default)** | **0** | **0** |

Real sensor noise is usually heavier-tailed than Gaussian. With `persist=1`, a 5 Hz sensor would raise about 30 false alerts an hour. The cost of `persist=2` is that a single-sample glitch goes unreported; set `persist: 1` on fields where those matter. Detector state is in memory, so after a restart each field re-learns for `warmup` samples, and episodes left open by the previous run are closed in the database.

## Dashboard tiles

| Tile | Shows |
|---|---|
| `stat` | current value, threshold status, sparkline |
| `meter` | value against a min–max range with warn/critical colouring |
| `line` | up to 4 fields. **Live**, or a historical range (1 h – 30 d) drawn as bucket averages with a min–max band. Anomaly episodes appear as red markers, explained in the hover tooltip |
| `stats` | mean, min, max, std and sample count over 1 h – 30 d |
| `anomalies` | anomaly log for one sensor or all, open episodes first, live |
| `state` | text or on/off with a "normal" value |

The header shows a live count of active anomalies. For `stat`/`meter` thresholds, set `warn` and `crit`; if `crit < warn`, low values are treated as bad. Fields in one `line` tile share a y-axis, so only group fields on the same scale.

### Adding a tile type

Tiles live in `web/tiles.js`. Register one, and it appears in the "Add tile" dialog:

```js
registerTile('big-number', {
  label: 'Big number',
  options: [{ key: 'decimals', label: 'Decimals', type: 'number' }],
  create(el, cfg, ctx) {   // ctx: sensor, unit(f), history(f, n), api(path), invalidate()
    const d = document.createElement('div'); d.className = 'value'; el.append(d);
    let v;
    return {
      update(t, value) { v = value; },                 // record only; called per reading
      render() { d.textContent = v?.toFixed(cfg.options?.decimals ?? 0); }, // ≤ once per frame
      anomaly(ev) {},                                  // optional: anomaly episodes for this sensor
    };
  },
});
```

Option types are `number`, `text` and `select` (`choices: [[value, label]]`). `sensorOptional: true` lets a tile watch all sensors, and `noField: true` hides the field picker.

## Efficiency, by design

| Choice | Effect |
|---|---|
| Fixed ring buffer per field (`int64` ts + `float32` value) | 12 B/point, allocated once, never grows; bounded by sensors × fields × points |
| Database writes batched (500 rows/statement), off the ingest path | thousands of writes/s on SQLite; ingestion never waits on disk |
| 1-minute rollups maintained at write time | a 30-day chart reads ≤ 43,200 rollup rows per field, not millions of raw rows |
| Integer series ids, clustered `(series, ts)` key | small rows; range scans are sequential |
| SQLite: WAL, 2 MB page cache per connection, max 4 connections | reads don't block writes; bounded memory |
| Anomaly detection: EWMA mean/variance per field | O(1) per reading, no history kept |
| One JSON encode per message, shared by all SSE clients; slow clients drop | fan-out cost doesn't grow with clients; ingestion never stalls |
| Columnar API responses | smaller payloads; map straight to typed arrays |
| Browser: typed arrays, ≤ 1 render per frame, canvas, no framework; historical tiles refresh no faster than one bucket | constant client work at any message rate |
| Hidden tab closes its stream | zero cost when nobody is looking |

**Measured in this container**, all with the database and anomaly detection on:

| Test | Result |
|---|---|
| 204 sensors at ~5 Hz (~1,000 readings/s over MQTT), SQLite | **5.8% of one CPU core, 30 MB RSS**, 1,019 rows/s written, 0 dropped |
| Bulk write, 2.6M rows (30 days at 1 Hz) | SQLite 158k rows/s (26 B/row on disk) · Postgres 16 86k rows/s |
| 30-day chart, 1 h buckets (rollups) | SQLite 40 ms · Postgres 19 ms |
| 24 h chart, 5 min buckets (rollups) | SQLite 1.7 ms · Postgres 1.2 ms |
| 30-day stats (rollups) | SQLite 16 ms · Postgres 10 ms |
| Same 30 days from raw rows instead (30 s buckets) | ~2.1 s on both, which is why rollups exist; the API refuses requests over 10,000 buckets |
| SIGTERM right after 300 REST writes | all 300 rows present after restart |

Without the database (`-db ""`): 20 MB RSS and 2.2% CPU at the same 1,000 msgs/s.

## Limits and next steps

- **Auth is a single shared token.** Reads (dashboard, stream, analytics) are open. Put it behind a reverse proxy with TLS, or add per-device credentials, before exposing it beyond a LAN.
- **Anomalies are statistical, not semantic.** The detector knows "unusual for this field", not "bad for this machine". Use `range` rules for known limits. A multi-sensor model (e.g. the ML.NET model in this repo, subscribed to `iot/#`) can publish its own findings back as a sensor.
- **No notifications yet.** Anomalies go to the dashboard, the database and MQTT. Email/Slack/webhook delivery would be a small subscriber on `iot-events/anomaly/#` or an `OnEvent` hook.
- **Statistics are mean/std/min/max.** No percentiles: they don't merge across rollups, so they would need raw scans or sketches.
- **The embedded broker is a single node.** To use an existing broker (Mosquitto, EMQX), run with `-mqtt ""` and add a small subscriber that calls `Pipeline.Handle`.
- **Deleting a sensor keeps its database history.** Re-creating the same id continues the series.
