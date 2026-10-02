# IoT Hub

A reusable sensor platform in one Go binary of ~16 MB (stripped):

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
| `-notify` (repeatable) | `IOTHUB_NOTIFY` (space-separated) | — | alert targets, `kind=URL` (see Notifications) |
| `-notify-secret` | `IOTHUB_NOTIFY_SECRET` | — | HMAC key for generic webhook signatures |
| `-notify-kinds` | `IOTHUB_NOTIFY_KINDS` | all | e.g. `range,stale` |
| `-notify-resolved` | `IOTHUB_NOTIFY_RESOLVED` | `true` | also send "resolved" messages |
| `-notify-cooldown` | `IOTHUB_NOTIFY_COOLDOWN` | `10m` | hold repeats for the same sensor/field/kind |
| `-notify-per-minute` | `IOTHUB_NOTIFY_PER_MINUTE` | `20` | global cap |
| `-report-to` (repeatable) | `IOTHUB_REPORT_TO` (space-separated) | | shift report targets, same kinds as `-notify`; sent at every shift change |
| `-backup-dir` | `IOTHUB_BACKUP_DIR` | — (off) | scheduled backups: SQLite snapshot + config. Put it on another disk |
| `-backup-every` | `IOTHUB_BACKUP_EVERY` | `24h` | time between backups |
| `-backup-keep` | `IOTHUB_BACKUP_KEEP` | `7` | backups of each kind to keep |
| `-tenancy` | `IOTHUB_TENANCY` | `single` | `multi` = SaaS mode, see [Multi-tenant](#multi-tenant-saas-mode) |
| `-master-key` | `IOTHUB_MASTER_KEY` | — | encrypts device keys at rest |
| `-allow-private-targets` | `IOTHUB_ALLOW_PRIVATE_TARGETS` | false | multi: let tenants notify internal addresses (SSRF guard off) |
| `-tenant-rate` / `-tenant-daily` / `-tenant-devices` | `IOTHUB_TENANT_RATE` / `_DAILY` / `_DEVICES` | 100 / unlimited / 1000 | default tenant quotas (multi) |
| `-public-url` | `IOTHUB_PUBLIC_URL` | — | dashboard link included in messages |
| `-shifts` | `IOTHUB_SHIFTS` | `06:00,14:00,22:00` | shift start times for OEE `from=shift` (empty disables) |
| `-tz` | `IOTHUB_TZ` | system | plant time zone, e.g. `Africa/Harare` |
| `-connectors` | `IOTHUB_CONNECTORS` | — | Modbus / OPC UA connectors file (see PLCs and meters) |
| `-token` | `IOTHUB_TOKEN` | — | admin token; setting it turns authentication on (see Security) |
| `-auth-file` | `IOTHUB_AUTH_FILE` | `data/devices.json` | per-device credentials (hashes only, mode 0600) |
| `-public-read` | `IOTHUB_PUBLIC_READ` | `false` | allow reads without a token (writes still need one) |
| `-tls-cert` / `-tls-key` | `IOTHUB_TLS_CERT` / `_KEY` | — | PEM files; enable HTTPS and MQTT over TLS |
| `-mqtts` | `IOTHUB_MQTTS` | `:8883` | MQTT over TLS address (when a certificate is set) |

## Security

With no `-token` and no credentials file, the hub is **open**, and it logs a warning saying so. Setting `-token` turns authentication on for HTTP and MQTT. That token is the **superadmin** (the platform operator; in single-tenant mode simply the admin). Everything else gets its own credential:

| Role | HTTP | MQTT | Typical holder |
|---|---|---|---|
| `superadmin` | everything in every tenant, plus tenant management (multi-tenant) | anything | the platform operator (`-token`) |
| `admin` | everything, including devices and dashboard layout | publish/subscribe anything | you |
| `operator` | read; acknowledge and shelve alarms, write notes | subscribe only | shift staff, supervisors |
| `service` | read; ingest and define **its** sensors | subscribe all; publish its sensors | the ML.NET bridge (`*-ml`) |
| `device` | ingest to **its** sensors only | publish its sensors only | a PLC gateway, an ESP32 |
| `viewer` | read only | subscribe only | a wall screen, a supervisor |

"Its sensors" are glob patterns such as `env-*`, `line3-*` or `*-ml`. A device that is compromised can only write the sensors it was given. It can't read anything, spoof anomaly events, or touch the dashboard.

```bash
# create (admin only); the token is shown once
curl -X POST https://hub:8443/api/devices -H "Authorization: Bearer $ADMIN" \
     -d '{"id":"env-node-1","role":"device","sensors":["env-*"],"note":"roof"}'
curl https://hub:8443/api/devices -H "Authorization: Bearer $ADMIN"                 # list (no secrets)
curl -X DELETE https://hub:8443/api/devices/env-node-1 -H "Authorization: Bearer $ADMIN"  # revoke
```

- **Tokens.** Each is 256 bits of randomness, stored only as a SHA-256 hash.
- **Revocation** takes effect immediately and also drops the device's live MQTT sessions.
- **Auth can't silently switch off.** Once a credentials file exists, removing the last device does not turn authentication off.
- **MQTT** username = device id, password = token. The admin token accepts any username.
- **Dashboard.** It asks for a token once, and `/api/login` stores it in an **HttpOnly, SameSite=Strict** cookie (Secure under TLS), so page scripts never see it.
  - Cookie-authenticated writes also need an `X-Requested-With: iothub` header, which another site can't send without a CORS preflight. The hub never approves one.
  - Viewers don't see the Edit button.
- **Headers.** Every response carries a strict Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`, and HSTS under TLS.
- **`/api/health` stays public** for load balancers. It reveals counts, not data.

### Device keys and SAS tokens (as in Azure IoT Hub)

An identity can have a **token**, a **primary/secondary key pair**, or both (`"auth": "token" | "keys" | "both"`). With keys, the device (or your backend) signs short-lived **SAS tokens**. A leaked SAS token expires on its own, and the keys never leave the device.

```bash
curl -X POST https://hub:8443/api/devices -H "Authorization: Bearer $ADMIN" \
     -d '{"id":"pump-1","role":"device","sensors":["pump-1"],"auth":"keys"}'
# → primaryKey, secondaryKey, connectionString:
#   HostName=hub:8443;TenantId=default;DeviceId=pump-1;SharedAccessKey=…
```

| | |
|---|---|
| SAS format | `SharedAccessSignature sr={tenant}%2Fdevices%2F{id}&sig={sig}&se={unix expiry}` |
| Signature | `base64(HMAC-SHA256(base64decode(key), urlencode(resource) + "\n" + expiry))`, the same construction as Azure IoT Hub, so its client libraries and samples can generate tokens |
| HTTP | `Authorization: SharedAccessSignature sr=…` (or `Bearer <token>`) |
| MQTT | password = the SAS token (or the token) |
| Lifetime | at most 366 days |

| Endpoint (admin) | |
|---|---|
| `POST /api/devices/{id}/sas {"ttl":"24h"}` | issue a SAS token from the primary key, for devices that can't sign |
| `POST /api/devices/{id}/rotate {"which":"primary"}` | new primary key (or `secondary`, `token`); the other key keeps working, so devices move over without downtime |
| `GET /api/devices/{id}/keys` | show the keys and connection strings (audited) |
| `PATCH /api/devices/{id} {"disabled":true}` | disable without deleting; also `sensors`, `note`, `expires` |
| `POST /api/devices` with `"expires": <unix ms>` | a token that stops working at that time |

Disabling, rotating, narrowing the sensors or changing the expiry drops the device's live MQTT sessions at once.

**Keys at rest.** Keys must be stored to verify signatures. Set `-master-key` (`IOTHUB_MASTER_KEY`, a long random secret kept outside the data directory) and they are encrypted with AES-256-GCM. Existing plain keys are encrypted on the next start. Without it, keys are stored as is in the credentials file (mode 0600).

**TLS.** Use a real certificate if the hub has a DNS name. Otherwise make a small private CA once:

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 -subj "/CN=Plant IoT CA" -keyout ca.key -out ca.crt
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=iothub" -keyout hub.key -out hub.csr
printf "subjectAltName=DNS:iothub,DNS:localhost,IP:192.168.1.10\nextendedKeyUsage=serverAuth\n" > ext.cnf
openssl x509 -req -in hub.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 -extfile ext.cnf -out hub.crt
iothub -token "$(openssl rand -base64 32)" -tls-cert hub.crt -tls-key hub.key -mqtt "" -http :8443
```

Install `ca.crt` in each client's trust store: Windows *Trusted Root*, Linux `/usr/local/share/ca-certificates` + `update-ca-certificates`, or the device firmware. `-mqtt ""` turns plain MQTT off; while it stays on, the hub warns that tokens cross the network in clear text.

**Tested end to end:** a TLS-only hub with a private CA in the OS store. The emulator posted over HTTPS with a `device` token, the ML.NET bridge connected over MQTT/TLS with a `service` token, and a browser signed in with a `viewer` token. The automated tests also cover:
- the role matrix
- MQTT topic ACLs (a device publishing outside its patterns, or to `iot-events/…`, is dropped)
- revocation kicking live sessions
- a username/id mismatch being rejected
- an untrusted certificate failing
- the CSRF header requirement

## Multi-tenant (SaaS) mode

`-tenancy multi` turns one hub into a platform hosting many customers. Each **tenant** is an isolated hub, like one Azure IoT Hub instance per customer:
- its own sensors, dashboard and live buffers
- its own anomaly detector and alarm state (acks, shelves, notes, audit)
- its own live stream and backups
- its own database: a SQLite file, or a PostgreSQL schema

The superadmin (`-token`) creates tenants, sets their quotas and decides who issues device credentials.

```bash
iothub -tenancy multi -token "$SUPER" -master-key "$MASTER" -db sqlite:readings.db \
       -tls-cert hub.crt -tls-key hub.key -mqtt "" -http :8443
S="Authorization: Bearer $SUPER"

curl -X POST https://hub:8443/api/admin/tenants -H "$S" -d '{"id":"acme","name":"Acme Mining",
      "quota":{"messagesPerSecond":200,"messagesPerDay":5000000,"maxSensors":500,"maxDevices":200,"rawRetentionDays":30}}'
# Device credentials for the tenant (only the superadmin, unless deviceSelfService is on):
curl -X POST https://hub:8443/api/devices -H "$S" -H "X-Tenant: acme" \
     -d '{"id":"pump-1","role":"device","sensors":["pump-*"],"auth":"keys"}'
curl -X POST https://hub:8443/api/devices -H "$S" -H "X-Tenant: acme" -d '{"id":"ops","role":"admin"}'   # the customer's admin
curl https://hub:8443/api/admin/tenants -H "$S"                       # usage: sensors, devices, messages today, rejected
curl -X PATCH https://hub:8443/api/admin/tenants/acme -H "$S" -d '{"status":"suspended"}'
curl -X DELETE 'https://hub:8443/api/admin/tenants/acme?confirm=acme' -H "$S"   # erases data, credentials, backups
```

**How a request finds its tenant:**

| | |
|---|---|
| HTTP, tenant user | from the credential. Naming another tenant (`X-Tenant`, `?tenant=`) is refused with 403 |
| HTTP, superadmin | `X-Tenant: acme` header, or `?tenant=acme` (for EventSource) |
| MQTT | username `{tenant}/{id}`; topics `{tenant}/iot/{sensor}[/{field}]` and `{tenant}/iot-events/…` |
| MQTT subscriptions | must start with the caller's own tenant: `acme/#` is allowed; `#`, `+/iot/#` and `$SYS/#` are refused |
| Dashboard | the same page; signing in with a tenant credential shows that tenant |

**Isolation, in layers:**
- **The gateway** authenticates once and hands the request only to the caller's tenant.
- **Each tenant's API server** checks the tenant again, so a gateway bug alone can't leak.
- **Separate runtimes.** Tenants share no stores, streams or caches.
- **Separate databases.** SQLite tenants have separate files under `data/tenants/{id}/`. PostgreSQL tenants have separate schemas (`t_{id}`), selected with `search_path`, so every query is confined to one schema.
- **No adopted leftovers.** A new tenant is refused if a data directory or schema with its name already exists, so it can never adopt an old tenant's data.

**Quotas** (Azure tiers, roughly). Each field is 0 for the platform default and −1 for unlimited:

| Quota | Enforced | Default |
|---|---|---|
| `messagesPerSecond` | token bucket with a 2 s burst; excess gets HTTP 429 (MQTT: dropped) | `-tenant-rate` 100 |
| `messagesPerDay` | count per UTC day | `-tenant-daily` unlimited |
| `maxSensors` | new sensors refused (507) | `-max-sensors` |
| `maxDevices` | new identities refused (507) | `-tenant-devices` 1000 |
| `rawRetentionDays`, `rollupRetentionDays` | pruning | `-raw-retention`, `-rollup-retention` |

Quota changes apply at once, without a restart. One tenant flooding messages is rejected before any work is done, so it can't slow the others.

**Suspension** keeps the data but drops the tenant's MQTT sessions and live streams and refuses its API and sign-in. Resuming restores everything.

**Single-tenant options** (`-public-read`, `-connectors`, `-notify`, `-report-to`) are refused in multi-tenant mode:
- **PLC connectors** belong on an edge hub at the plant, which forwards to its tenant. A cloud server can't reach plant networks.
- **Notifications and reports** are configured per tenant through the API.

**Tested** (`internal/gateway`, SQLite and PostgreSQL):
- **Two tenants, the same names.** `acme` and `globex` use the same sensor names and device ids. Each sees only its own values, stats and history.
- **Naming another tenant** is refused on every route tried (reads and writes, `X-Tenant` and `?tenant=`).
- **No leaks** through device listings, config exports, live streams or MQTT:
  - a username naming another tenant is refused;
  - a publish into another tenant is dropped;
  - `#`, `+/iot/#`, the other tenant's topics and `$SYS` are refused.
- **Suspension** drops MQTT sessions and the data survives a resume. **Deletion** erases the data directory and tokens, and the id can be reused cleanly.
- **Quotas:** rate (10 of 30 accepted at 5/s, the rest 429), sensors and devices; raising a quota applies at once.
- **Self-service** off and on.
- **Mutation check:** with the gateway's tenant check removed, the server's own check still blocks every request. With both removed, or with the broker's topic check removed, the test fails. So the test can detect a leak, and the layers are independent.
- **PostgreSQL:** separate `t_acme` / `t_globex` schemas; an existing schema is refused instead of adopted. That check was added after the test found a new tenant inheriting a leftover schema's readings.
- **The built binary:** tenant creation, a keys device, SAS ingest, encrypted keys in the file, and a restart with tenant, data and SAS intact.

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

## PLCs and meters (Modbus, OPC UA)

The hub reads industrial equipment directly, with no gateway software. Connectors are declared in a JSON file passed with `-connectors`; see [`examples/connectors.json`](examples/connectors.json). Each connector feeds a hub sensor through the normal pipeline, so PLC tags get:
- live tiles, history, analytics and anomaly detection
- the MQTT republish on `iot/{sensor}`, so the ML.NET bridge can score them too

Units and labels from the file are added to the sensor definition, without overwriting anything an admin has set.

**Modbus** (`"modbus": [...]`)
- **Connection:** `url` is `tcp://host:502`, `rtu:///dev/ttyUSB0` (with `baud`, `parity`, `stopBits`), or `rtuovertcp://gateway:4001`.
- **Units:** one connection can serve many `units` (slave ids), each becoming one sensor. That covers an RS-485 bus of meters, or a gateway with many drops.
- **Registers:** `table` is `holding`, `input`, `coil` or `discrete`. `address` is the **0-based protocol address**, so holding register 40001 is address 0.
- **Types:** `type` is `int16`, `uint16`, `int32`, `uint32`, `float32`, `int64`, `uint64` or `float64`. `order` is `ABCD` (default), `CDAB` (word-swapped, common on Modicon PLCs and energy meters), `BADC` or `DCBA`. `bit: n` reads one bit of a status word.
- **Scaling:** `scale` and `offset` give engineering units, e.g. `int16 × 0.1`.
- **Efficiency:**
  - Registers are grouped into the fewest reads: up to 125 registers per request, bridging gaps of up to `maxGap` (default 8).
  - If a PLC rejects a span because it covers unmapped addresses, the connector reads those registers singly from then on and logs it once.
  - Each poll is one round trip per block, not one per tag.

**OPC UA** (`"opcua": [...]`)
- **Subscriptions:** monitored items, so the server pushes changes; there's no polling.
- **Deadband:** a `deadband` (absolute) is sent to the server, so suppressed changes never cross the network.
- **Heartbeat:** every `heartbeat` (default 60 s) the connector *reads* all nodes. Unchanged tags never look stale, and the read corrects servers that implement deadband against the previous sample rather than the last report. The test server does exactly that, and without the read a slowly drifting value froze.
- **Sample mode:** `sample: true` reads every `interval` instead, giving evenly spaced samples for unbiased averages.
- **Quality:** values with bad or uncertain status are dropped rather than recorded.
- **Security:** `securityPolicy` is `None` or `Basic256Sha256` (`Sign` / `SignAndEncrypt`), with optional `username`/`password`.
  - For secure policies a client certificate is generated next to the credentials file (`data/opcua-<name>.crt`). Trust it once on the PLC; S7-1500 and most servers list rejected certificates for approval.
  - Or set `certFile`/`keyFile`.

**Report-by-exception (Modbus).** By default every poll is stored. Setting `onChange: true` or a `deadband` on a field stores it only when it changes, plus once per `heartbeat` (default 60 s).
- This greatly reduces rows for slow tags such as setpoints, states and energy counters.
- The trade-off: averages become sample-weighted, so busy periods weigh more. Leave it off for process values you will average.

**Reliability.**
- An unreachable device is retried with backoff (1 s up to 30 s) and logged once per outage, then once on recovery. The hub's `stale` detector raises an anomaly if a PLC stays silent.
- `GET /api/connectors` reports each connector: connected, last error, reads, values published and suppressed.

**Tested here:**
- **Modbus**, against an independent implementation (pymodbus 3.6) acting as a PLC:
  - a sparse map that rejects gapped reads
  - two units on one connection, CDAB floats, scaled int16, a status bit, coils and input registers
  - the PLC being killed and restarted
- **OPC UA**, against asyncua 2.0:
  - unsecured, and `Basic256Sha256`/`SignAndEncrypt` with the generated client certificate
  - absolute deadband, heartbeat reads and sample mode
- **Resources:** five connectors (two Modbus units at 2 Hz, four OPC UA sessions) ran in **24 MB RSS at 1.1% CPU**.
- **CI:** the automated tests cover decoding for every type and byte order, block planning, the gap fallback, report-by-exception and reconnection.

**Not tested here:**
- Modbus RTU on a real serial port (no hardware in this environment).
- An OPC UA server that *enforces* client-certificate trust: asyncua accepts any client certificate, so the approval step on a real PLC is unverified.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/sensors` | all sensors with `last` values and `lastSeen` |
| GET / PUT / DELETE | `/api/sensors/{id}` | definition: `name`, `kind`, `location`, `fields{name:{label,unit,min,max,detect}}` |
| POST | `/api/sensors/{id}/data[/{field}]` | ingest, returns 202 |
| GET | `/api/sensors/{id}/history?field=&limit=&since=` | live ring buffer, columnar `{t, v}` |
| GET | `/api/sensors/{id}/series?field=&from=&to=&bucket=` | bucketed `{t, n, min, max, avg}`; `bucket` = `auto` (≤600 points), or a duration like `5m` |
| GET | `/api/sensors/{id}/stats?field=&from=&to=` | `{n, min, max, mean, std}` |
| GET | `/api/sensors/{id}/forecast?field=&horizon=6h&history=&threshold=&side=` | projection with an 80 % band, skill vs naive, and when it crosses the field's alert limits |
| GET | `/api/anomalies?sensor=&from=&to=&limit=&active=1` | anomaly episodes, newest first |
| GET / PUT | `/api/dashboard` | layout JSON (opaque to the server) |
| GET | `/api/stream[?sensors=a,b]` | SSE: readings as `data: {"s","t","v"}`; anomalies as `event: anomaly` |
| GET | `/api/reports?shift=previous\|current&format=json\|html\|text` | shift report (also `from`/`to`); see [Shift reports](#shift-reports) |
| POST | `/api/reports/send` | send the previous shift's report to the `-report-to` targets now (admin) |
| GET / POST | `/api/config` | export / import the configuration; see [Backup and restore](#backup-and-restore) (admin) |
| GET / POST | `/api/backups`, GET `/api/backups/{name}` | list / take / download backups (admin) |
| GET | `/api/connectors` | PLC connector status |
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

**After a restart** the hub reloads live state from the database before it accepts any data, so tiles show the last known values straight away. It restores:
- each field's newest value, with booleans and text restored as such
- up to `-points` recent readings per field, so live charts and sparklines are full

It also warms each field's anomaly baseline from that history, so spike detection works on the first live readings instead of re-learning for 30 samples. The silent-sensor check counts the restart itself as "last seen", so time the hub spent down never raises a stale alarm for every sensor at once.

Restore takes about **0.4 s for 600 series × 1,024 points** (614k rows) on both SQLite and Postgres. Each series is one index range scan, newest first.

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

## Calculated fields

A field can be calculated from the sensor's other fields, either as a formula or as an integral over time. Results are stored, charted, forecast and alarmed like measured fields. Devices can't write them directly; the calculated value wins.

```bash
curl -X PUT https://hub:8443/api/sensors/press-1-power -H "Authorization: Bearer $ADMIN" -d '{
  "fields": {
    "power_kw":   { "unit": "kW",  "calc": { "formula": "sqrt(3) * voltage * current * 0.85 / 1000" } },
    "energy_kwh": { "unit": "kWh", "calc": { "integrate": "power_kw", "per": "1h" } } } }'
```

Formulas may use **measured** fields only, which keeps calculations free of chains and cycles. A formula over another calculated field, such as `energy_kwh * 0.11` for cost, is rejected. Integrals may use measured fields or formulas.

**Formulas:**
- **Operators:** `+ - * / % ^` and parentheses.
- **Functions:** `abs sqrt min max round(x, digits) clamp(x, lo, hi) if(cond, a, b)`.
- **Comparisons and logic:**
  - `< <= > >= = == != <>` and `and or not` (or `&& || !`) give 1 or 0, e.g. `if(level > 90 and pump = 0, 1, 0)`.
  - `and`/`or` short-circuit, so `flow > 0 and level / flow > 2` never divides by zero.
- **Inputs:** booleans count as 1/0.
- **Safety:** formulas are parsed by a small parser (no code is executed) and validated when the definition is saved.
- **Bad results:** a missing input, division by zero or a non-finite result produces *no* value rather than a wrong one.
- **When they run:** a formula recalculates only when one of its inputs arrives.

**Integrals** use the trapezoid rule:
- **Exact for linear changes,** e.g. 1 kW for 1 h gives exactly 1 kWh in testing.
- **Gaps:** a gap longer than `maxGap` (default 5 m) adds nothing, rather than assuming the last value held.
- **Restarts:** the total continues across restarts from the stored value.

Field names used in formulas can contain letters, digits, `_` and `.` (a `-` would read as minus).

## OEE (machine efficiency)

Overall Equipment Effectiveness = **availability × performance × quality**. Add an `oee` block to a machine's sensor:

```bash
curl -X PUT https://hub:8443/api/sensors/press-1 -H "Authorization: Bearer $ADMIN" -d '{
  "fields": { "running": {}, "cycles": {}, "rejects": {}, "break": {} },
  "oee": { "running": "running", "total": "cycles", "reject": "rejects",
           "plannedStop": "break", "idealCycleSec": 28 } }'
curl 'https://hub:8443/api/sensors/press-1/oee?from=shift&bucket=1h'   # also from=today, -24h, -7d, or ms/RFC 3339
curl 'https://hub:8443/api/oee?from=shift'                             # every machine, worst first
```

| Part | Formula | Tag |
|---|---|---|
| Availability | run time ÷ planned time | `running` (bool); optional `plannedStop` (bool) removes breaks and changeovers from planned time |
| Performance | ideal cycle × parts ÷ run time | `total` counter + `idealCycleSec` |
| Quality | good ÷ parts | `good` **or** `reject` counter (without either, Q is assumed 100 % and the result says so) |

**Rules**, chosen to be explainable on the shop floor:
- **State tags** hold their value until the next sample, for at most `maxHold` (default 5 m). Longer gaps are **no data**: excluded from planned time and reported as `coverage`, rather than guessed as running or stopped.
- **Counters** count positive steps from the last value *before* the window; a counter's first-ever value is a baseline, not production. A drop is a counter reset (PLC restart), so the new value counts from zero and the result is never negative.
- **Windows** are half-open `[from, to)` for both time and counts, so a part counted exactly at a shift change belongs to the new shift.
- **Warnings** appear in the result and the tile: performance over 105 % (the ideal cycle time is probably wrong), coverage under 90 %, and a machine that ran without counting.

**Dashboard:**
- **`oee` tile:** OEE with the 85 % / 60 % benchmarks (labelled, not colour alone), A/P/Q bars, parts and run time, hourly bars for the shift, and any warnings.
- **Shifts** come from `-shifts`, in the plant time zone `-tz`.

**Tested:**
- **Unit test:** a synthetic 8 h shift with a counter reset, a planned break and an unplanned stop reproduces the exact answer (A 0.8, P 0.9, Q 616/648, OEE 0.6844).
- **API test:** the same through the API gives OEE 0.6.
- **Demo:** a seeded live shift matched a hand calculation of planned time, run time and availability to the second.

## Forecasting

`/api/sensors/{id}/forecast` projects a field forward, and answers *when* it will reach a limit, e.g. "the diesel tank reaches 15 % in about 8 h (7 h 37 m – 8 h 17 m)". It runs in the hub, so there's no separate service. The method is exponential smoothing: Holt (level + damped trend), or additive Holt–Winters with a daily season once there are 3+ days of history.

**Model selection, and why to trust it:**
- **Parameters** are fitted by one-step error over the whole history.
- **The model family** (flat / trend / seasonal) is chosen by multi-step error on the most recent 20 %. A richer family must beat a simpler one by **10 %**.
  - Without that margin, holdout noise invents trends. In testing, a flat noisy series drifted 17 % (50 → 41) over the horizon, which would mean false "limit reached in N hours" warnings.
- **Skill.** Every response reports `skill = 1 − MAE / MAE(no-change forecast)`. The ETA tile labels the result *reliable* (skill > 0.2) or *low confidence*.
- **History handling:**
  - The bucket size adapts until at least 80 % of buckets hold real data; short gaps are interpolated.
  - Forecasts are clamped to the field's `min`/`max` (a tank never goes below 0 %).
  - The 80 % band assumes errors grow with the square root of the horizon. It's a guide, not a guarantee.

**Measured** (100 random seeds per case, in `internal/forecast`):

| Case | Result |
|---|---|
| Linear drain + noise, time to limit | median error **0.9 %**, p90 2.7 %, worst 6.2 % |
| Flat noise, false crossing of ±10 | **0 / 100** |
| Daily cycle + noise | Holt–Winters chosen 100/100, beats no-change 100/100 |
| End to end via the API (12 h of 1-min tank data) | ETA 7.98 h vs true 8.0 h |

**Dashboard:**
- **`eta` tile:** time to limit, with range and confidence. The limit defaults to the field's alert `low`/`high`.
- **`forecast` option on `line` tiles:** dashed projection, band, a "now" divider, the limit line, and the crossing point. Hover shows the forecast value and its likely range.

**Limits.** This extrapolates recent behaviour: a refill, a shift change, or a process change it has not seen will make it wrong until the data shows it. It's for operational ETAs (tanks, filters, temperature drift), not process modelling. Process-specific models belong in the ML.NET service once real data exists.

## ML.NET integration

The access-control classifier in `prediction.Server` is connected through MQTT. Neither side calls the other directly:

```
AccessControlEmulator ─POST─► hub /api/sensors/access-1/data ─► iot/access-1 (MQTT)
                                                                     │
prediction.Server  Services/IotHubBridge.cs ◄────────────────────────┘
   PredictionEnginePool.Predict(...)
   └─► iot/access-1-ml  {"prediction":"Abnormal","confidence":0.92,"risk":0.92,"p_Normal":0.08,"p_Abnormal":0.92,"ts":…}
                       └─► hub: charts, database, anomaly when risk > 0.5
```

- **What gets scored.** The bridge subscribes to `iot/+` and scores any JSON message that has `AccessMethod`, `AccessStatus` and `LocationID` (configurable). Any number of access sensors works with no per-sensor setup.
- **Input mapping.** Payload fields map onto `ModelInput` by name, case-insensitively.
  - Missing numbers are passed as missing; the model's own `ReplaceMissingValues` step imputes them.
  - `Timestamp`, `HourOfDay` and `DayOfWeek` are derived from the reading time when absent. The hub turns a `Timestamp` field into the reading time, so it is usually absent from the republished payload.
- **Output.** Each event produces:
  - `prediction`: the class, as text state
  - `p_<class>`: each class probability
  - `confidence`: the top probability
  - `risk`: 1 − P(`Normal`)
- **Self-registration.** The first result per sensor registers `{sensor}-ml` with the hub: name, 0–1 ranges, and a **range rule on `risk`**. That rule turns "the model thinks this access is abnormal" into a hub anomaly episode. Spike detection is off for probabilities, which jump by nature.
- **SignalR.** Results are also sent on the existing SignalR `Prediction` message, so current clients keep working.

Configuration lives in `prediction.Server/appsettings.json` under `IotHub`, or in environment variables like `IotHub__Host`:

| Key | Default | |
|---|---|---|
| `Enabled` | `true` | |
| `Host` / `Port` | `localhost` / `1883` | hub MQTT broker |
| `Username` / `Token` | `mlnet-bridge` / — | a hub credential with role `service` and sensors `["*-ml"]` |
| `UseTls` | `false` | MQTT over TLS (set `Port` to 8883); certificate checked against the OS trust store |
| `CheckCertificateRevocation` | `false` | online revocation check; private CAs usually have no revocation list |
| `RequiredFields` | `AccessMethod, AccessStatus, LocationID` | message must have these to be scored |
| `NormalLabel` | `Normal` | the model's classes are `Normal`, `Abnormal` |
| `RiskThreshold` | `0.5` | risk above this opens an anomaly |
| `HubUrl` | `http://localhost:8080` | for self-registration; empty skips it |

The bridge reconnects with backoff (up to 60 s) and logs only the first failure, so it can start before the hub. The emulator posts to the hub when `IOTHUB_URL` is set (default `http://localhost:8080`), with `EMULATOR_INTERVAL_SECONDS` controlling its rate.

**Running everything:** `dotnet run --project prediction.AppHost` starts the hub (via `go run`, so Go must be installed), `prediction.Server` with the bridge, the emulator and the React client under Aspire.

Suggested tiles: a `state` tile on `access-1-ml.prediction` (normal value `Normal`), a `line` tile on `risk` (Y 0–1), and an `anomalies` tile on `access-1-ml`.

## Alarm handling (acknowledge, shelve, audit)

Anomaly episodes are alarms that people act on, loosely following ISA-18.2:

- **Acknowledge**, with a verdict and an optional note. The verdict is *confirmed* (a real problem), *false alarm*, or *expected* (maintenance, changeover). The header badge counts **unacknowledged** active alarms: what still needs a person. Acknowledged ones stay visible until they clear.
- **Notes:** a journal per episode ("replaced the float switch"), shown in the anomaly log.
- **Shelve** a nuisance alarm for one sensor/field/kind, or a whole sensor:
  - **Limits:** a **reason is required**, and the maximum is **7 days**, so a forgotten shelf can't silence a sensor forever.
  - **While shelved:** episodes are still recorded and shown greyed, but don't notify or count as active.
  - **Ending:** shelves expire on their own or can be removed early.
- **Audit log** (`GET /api/audit`, admin): who acknowledged, shelved or noted what, plus configuration changes (sensor definitions, dashboard, devices).
- **Training labels** (`GET /api/labels.csv?from=-90d`): every acknowledged episode with its verdict, ready to join with readings when a model is trained on real data. Free text is defused against spreadsheet formula injection.

The `operator` role can do all of this but can't change configuration; viewers see the state only. Everything persists in the database and survives restarts; without a database it is kept in memory.

| Method | Path | Role |
|---|---|---|
| POST | `/api/anomalies/{id}/ack` `{"verdict":"false_alarm","note":"…"}` | operator |
| POST / GET | `/api/anomalies/{id}/notes` | operator / viewer |
| GET / POST | `/api/shelves` `{"sensor","field","kind","duration":"8h","reason"}` | viewer / operator |
| DELETE | `/api/shelves/{key}` | operator |
| GET | `/api/audit?from=-30d` | admin |
| GET | `/api/labels.csv?from=-90d` | viewer |

## Notifications

Anomaly episodes are sent to people when they open and, by default, when they resolve. Targets are `kind=URL`:

| Kind | URL | Message |
|---|---|---|
| `webhook` | any https endpoint | full JSON (event, status, title, text). `X-IoTHub-Signature: sha256=<HMAC of body>` when `-notify-secret` is set |
| `slack` | Slack incoming webhook | text |
| `teams` | Teams **Workflows** webhook ("Post to a channel when a webhook request is received") | Adaptive Card with a dashboard button |
| `discord` | Discord channel webhook | text |
| `telegram` | `https://api.telegram.org/bot<TOKEN>/sendMessage?chat_id=<ID>` | text |
| `email` | `smtp://user:pass@host:587?from=hub@x.com&to=a@x.com,b@x.com` (STARTTLS), or `smtps://…:465` | plain text |

```bash
IOTHUB_NOTIFY="teams=https://prod-00.westeurope.logic.azure.com/workflows/… email=smtp://alerts:PASS@smtp.office365.com:587?from=alerts@plant.co&to=maintenance@plant.co" \
IOTHUB_PUBLIC_URL=https://iothub.plant.local:8443 iothub …
curl -X POST https://hub:8443/api/notifications/test -H "Authorization: Bearer $ADMIN"   # try every target now
curl https://hub:8443/api/notifications -H "Authorization: Bearer $ADMIN"              # sent / failed / last error
```

**Alarm-storm protection**, because an alert channel that floods gets muted:
- **Cooldown.** A new episode for the same sensor, field and kind within the cooldown is held, not sent. The next message that does go out says how many were held.
- **Global cap.** A token bucket allows `-notify-per-minute` messages in total.
- **Resolved messages** are sent only for episodes whose opening was sent.

**Delivery:**
- **Never blocks ingestion.** Messages go through a bounded queue.
- **Retries.** 5xx and 429 responses are retried at 1 s and 4 s; a 4xx is a configuration error and is not retried.
- **Email.** It has a 30 s deadline, and credentials only travel over TLS.
- **Secrets stay hidden.** URLs (which contain secrets) are shown redacted everywhere. Prefer the environment variable to the flag, so they don't appear in process lists.

**Tested:**
- **End to end:** a real hub sending range anomalies to a local receiver. Signatures verified; cooldown hold; no orphan "resolved"; the test endpoint.
- **Unit tests:** retry vs no-retry, the rate cap, every chat payload shape, and SMTP including AUTH against a fake server.
- **Not tested:** delivery to the real Slack, Teams, Discord and Telegram services, which aren't reachable from here.

### Settings per tenant, and escalation

Notification targets, escalation, shift reports, shifts and the time zone are configured through the API by the tenant's admin. In single-tenant mode the `-notify`, `-report-to`, `-shifts` and `-tz` flags seed these settings. Once an admin saves settings, `settings.json` is the source of truth.

```bash
A="Authorization: Bearer $ADMIN"
# Name each target once; its URL (which holds a secret) is never shown back.
curl -X PUT https://hub:8443/api/settings/targets/ops-teams -H "$A" -d '{"spec":"teams=https://prod-00.westeurope.logic.azure.com/workflows/…"}'
curl -X PUT https://hub:8443/api/settings/targets/manager   -H "$A" -d '{"spec":"email=smtp://alerts:PASS@smtp.office365.com:587?from=alerts@plant.co&to=manager@plant.co"}'
curl -X POST https://hub:8443/api/settings/targets/ops-teams/test -H "$A"
curl -X PUT https://hub:8443/api/settings -H "$A" -d '{
  "notify":     {"targets":["ops-teams"], "resolved":true, "cooldownMin":10},
  "escalation": {"levels":[{"afterMin":15,"targets":["ops-teams"]},{"afterMin":60,"targets":["manager"]}], "repeatMin":60},
  "reports":    {"targets":["manager"]},
  "shifts": ["06:00","14:00","22:00"], "timeZone": "Africa/Harare", "webhookSecret": "…" }'
curl https://hub:8443/api/settings -H "$A"     # targets redacted, with sent/failed/last error; recent escalations
```

**Settings rules:**
- **Validated whole and applied live.** Notifier, escalation policy, report schedule, shifts and time zone switch over without a restart. Alerts already queued on the old notifier still go out.
- **Referential checks.** A target that something still uses can't be removed. Unknown fields are rejected.
- **Private file.** `settings.json` is mode 0600, because target URLs hold secrets.
- **Write-only secret.** `webhookSecret` is never returned.

**Escalation** re-notifies about alarms that are still **open, unacknowledged and not shelved**:
- **Levels.** Each level notifies its own targets after its delay: here the shift team after 15 min, the manager after 60.
- **Repeats.** With `repeatMin`, the last level is repeated until someone acknowledges.
- **Stopping.** Acknowledging, shelving or the alarm clearing stops it at once.
- **After a restart** (or a policy change), an alarm that is already old gets **one** message, for the highest level that is due, not a burst of every level.
- **Not persisted.** Escalation state is kept in memory, so a restart can repeat the current level once. A missed escalation would be worse than a duplicate.

**Blocking private addresses (multi-tenant).** Tenants choose where notifications go. Without a guard, a tenant could aim a "webhook" at the platform's own network (cloud metadata at `169.254.169.254`, databases, admin panels): server-side request forgery. In multi-tenant mode:
- **What is refused:** every notification connection (webhook, chat, SMTP) to loopback, private, link-local, CGNAT, unspecified or multicast addresses.
- **When the check runs:** when the address is dialled, after DNS resolution, so a name that resolves to an internal address (DNS rebinding) is caught too. Obvious cases are also refused when the target is saved, with a clear error.
- **No proxy:** connections go direct while the guard is on. `HTTP(S)_PROXY` is not used for notifications, since a proxy would hide the real destination.
- **Opting out:** `-allow-private-targets` turns the guard off, for on-premises platforms where tenants are trusted.

**Tested:**
- **Escalation:** levels at 15 and 60 min, a repeat after 30 min, stopping on acknowledge, one message after a restart, and shelved alarms never escalating.
- **Settings through the gateway:**
  - URLs and secrets are not echoed back;
  - six kinds of bad settings are refused, and an in-use target can't be removed;
  - a range alarm is delivered live to the tenant's webhook;
  - shifts and time zone apply at once (the current shift starts at 07:00/19:00 Harare time);
  - the file is 0600 and reloads after a suspend/resume.
- **Guard:**
  - an address table (`::ffff:127.0.0.1`, NAT64 and others) is classified correctly;
  - five private targets are refused through the API;
  - a target that passed configuration but points at a local server is stopped at dial time, for both HTTP and SMTP;
  - with the guard off, local delivery works.
- **Single-tenant binary:** `-notify` still delivers, and shows up as target `notify-1` in `/api/settings`.

## Stream jobs (as in Azure Stream Analytics)

A **job** is a small SQL-like query that runs continuously over incoming readings, aggregates them in time windows, and writes the results to a derived sensor, raises alarms, or posts to a webhook.

```sql
-- 5-minute average level per tank, as sensors avg-tank-1, avg-tank-2, …
SELECT avg(level) AS level_avg INTO [avg-{sensor}] FROM [tank-*]
GROUP BY sensor, TumblingWindow(minute, 5)

-- energy used per hour, updated every 15 minutes, from a meter's running kWh total
SELECT increase(kwh) AS kwh_last_hour INTO [plant.energy] FROM [meter-*]
GROUP BY HoppingWindow(minute, 60, 15)

-- alarm when a door opens more than 5 times in 10 minutes; it clears by itself
SELECT count(open) INTO alert FROM [door-3] WHERE value = 1
GROUP BY SlidingWindow(minute, 10) HAVING count > 5

-- post oven peaks above 250 °C to a named settings target
SELECT max(temp) INTO webhook:ops-teams FROM [oven-*]
GROUP BY sensor, TumblingWindow(second, 30) HAVING value > 250
```

```bash
curl -X PUT https://hub:8443/api/jobs/tank-avg -H "$A" -d '{"enabled":true,"lateness":"10s","query":"SELECT avg(level) INTO [avg-{sensor}] FROM [tank-*] GROUP BY sensor, TumblingWindow(minute, 5)"}'
curl -X POST 'https://hub:8443/api/jobs/test?from=-6h' -H "$A" -d '{"query":"…"}'   # dry run over stored history: what it would output
curl https://hub:8443/api/jobs -H "$A"         # status: in, filtered, out, late, dropped, errors, last output, open windows/alerts
curl -X DELETE https://hub:8443/api/jobs/tank-avg -H "$A"
```

| Clause | |
|---|---|
| `SELECT agg(field) [AS name]` | `avg min max sum count stddev first last delta increase`. `increase` sums the rises of a running total (counters, kWh) and counts a drop as a reset, so a meter restart doesn't produce a negative |
| `INTO` | `[sensor]` or `[prefix-{sensor}]` (`.field` optional): a derived sensor that works like any other (tiles, history, detection, MQTT). `alert`: a `rule` alarm per group while `HAVING` holds, which can be acknowledged, notified and escalated, and clears when it stops holding. `webhook:<target id>` |
| `FROM [glob]` | sensors by id or glob |
| `WHERE` | filter on `value` (or the field by name) |
| `GROUP BY [sensor,] window` | `TumblingWindow(unit, n)`, `HoppingWindow(unit, size, hop)`, `SlidingWindow(unit, n)`; units `second minute hour day` |
| `HAVING` | condition on `value`, `count` or the alias; `AVG(level) > 80` also works |

**Time and lateness:**
- **Event time.** Windows follow the readings' own timestamps.
- **Closing.** A window closes when readings show time has passed its end plus `lateness` (default 5 s), or, if readings stop, when the wall clock has.
- **Late readings** that arrive after that are counted as `late` and dropped.
- **Backfill.** Readings arriving in a burst with old timestamps (store-and-forward) are windowed correctly, because closing follows their timestamps, not arrival.

**Safety:**
- **No loops.** A job's output may not be any enabled job's input (one step, no chains), so jobs can't feed each other. This is checked when saving, including templated names like `{sensor}-5m`, and enforced again when writing.
- **Bounded memory.** Tumbling and hopping windows keep only running aggregates. Sliding windows keep their readings, up to 10 000 per group. A job holds at most 20 000 open windows. Hopping is limited to 60 windows per reading. Anything over a limit is counted as `dropped`, never unbounded.
- **Quotas.** Derived readings don't count against the tenant's message quota, but jobs do have a quota (`maxJobs`, default 50).

**Throughput** (measured, one core, race detector off):
- **Tumbling, grouped by sensor over 500 sensors:** 0.5 µs per reading (3 allocations).
- **Sliding, 10 000 readings in the window:** 1.1 µs.

The first versions measured 35 µs and 118 µs. Two fixes brought them down: closing windows only when one is due, rather than scanning on every reading; and keeping sliding aggregates incremental, with monotonic queues for min and max.

**Tested:**
- **Compiler:** 17 bad queries are refused with reasons, including a templated output that would feed its own input.
- **Windows and aggregates:**
  - tumbling per sensor, with in-lateness reordering and a late reading dropped;
  - hopping `increase` across a meter reset, closed by the wall clock, with each window checked against a hand calculation (4, 8, 6 kWh);
  - every aggregate against hand values.
- **Alerts, loop guard and webhooks:**
  - a sliding alert raises, then clears when readings stop;
  - removing a job clears its alarms;
  - the loop guard blocks an output that would feed another job;
  - webhook rows go out only when `HAVING` holds.
- **Incremental sliding aggregates** match a naive recomputation over 20 000 random pushes and evictions. A mutation check confirmed that test fails if the min queue is broken.
- **End to end through the gateway:**
  - a derived sensor from live readings;
  - a rule alarm that can be acknowledged;
  - a dry run that writes nothing;
  - chain and quota refusals;
  - jobs surviving a restart.

## Shift reports

At every shift change the hub sends a report for the shift that just ended to the `-report-to` targets. The report contains:
- **Machines:** OEE with availability, performance and quality, parts and rejects, run time, and any OEE warnings.
- **Energy:** consumption over the shift for every integrated field, e.g. `energy_kwh` (resets handled like counters).
- **Alarms:** total, unacknowledged and shelved; counts by kind; the longest episodes with their verdict and who acknowledged them.
- **Silent sensors:** sensors that went stale during the shift.

The numbers come from the same code as the dashboard tiles and `/api/sensors/{id}/oee`, so a report never disagrees with what operators saw.

```bash
IOTHUB_REPORT_TO="email=smtp://reports:PASS@smtp.office365.com:587?from=hub@plant.co&to=production@plant.co teams=https://…" \
IOTHUB_SHIFTS=06:00,14:00,22:00 IOTHUB_TZ=Africa/Harare IOTHUB_PUBLIC_URL=https://iothub.plant.local:8443 iothub …

curl 'https://hub:8443/api/reports?format=html' > last-shift.html                # print or save as PDF from the browser
curl 'https://hub:8443/api/reports?shift=current&format=text'                    # the shift so far
curl -X POST https://hub:8443/api/reports/send -H "Authorization: Bearer $ADMIN"  # check the targets now
```

| Target | Gets |
|---|---|
| `email` | HTML report with a plain-text alternative (multipart, quoted-printable) |
| `slack`, `teams`, `discord`, `telegram` | the text summary, shortened to each service's message limit |
| `webhook` | the full report as JSON under `report`, signed with `-notify-secret` |

**Behaviour:**
- **Timing.** The report is sent 5 s after each shift start in `-shifts`, so the last readings of the shift are in.
- **Separate from alerts.** Report targets are independent of the `-notify` alert targets and are not affected by the alert cooldown or rate cap.
- **Missed reports.** A report missed while the hub was down is not sent later; produce it from the API with `from`/`to`.
- **Without a database**, a report covers only what the in-memory buffers still hold. Use `-db` for full shifts.
- **Escaping.** Sensor names and alarm messages come from devices, so the HTML is built with Go's `html/template`, which escapes them.

**Tested:**
- **API test:** the report over a 2 h window matches the OEE endpoint (OEE 0.6) and the energy meter (1 kWh), in JSON, HTML and text.
- **Unit tests:** shift windows including the night shift either side of midnight; HTML escaping of a `<script>` sensor name, an `<img onerror>` alarm message and a `javascript:` link; chat clipping that never splits a multi-byte character; the multipart email against a fake SMTP server, with no line over the SMTP limit of 998 bytes.
- **End to end:** the built binary with a shift change set a minute ahead delivered the report to a local webhook 5 s after the change, covering exactly the shift that ended.
- **Not tested:** rendering in real mail clients (Outlook, Gmail), and delivery to the real chat services.

## Backup and restore

Two parts, because they change at different rates and restore differently:

| | Contents | How |
|---|---|---|
| **Configuration** | sensor definitions (fields, limits, detection rules, calculated fields, OEE), the dashboard layout, and optionally device credentials | `GET /api/config` → JSON; `POST /api/config` to restore |
| **Data** | readings, roll-ups, anomaly history, acknowledgements, notes, shelves, audit log | SQLite: online snapshots. PostgreSQL: `pg_dump` |

```bash
# Configuration, e.g. before an upgrade or to copy a set-up to another site
curl -H "Authorization: Bearer $ADMIN" 'https://hub:8443/api/config?devices=1' -o hub-config.json
curl -H "Authorization: Bearer $ADMIN" -X POST 'https://hub:8443/api/config?dryRun=1' --data-binary @hub-config.json  # what would change
curl -H "Authorization: Bearer $ADMIN" -X POST 'https://hub:8443/api/config' --data-binary @hub-config.json          # merge (default)
curl -H "Authorization: Bearer $ADMIN" -X POST 'https://hub:8443/api/config?mode=replace' --data-binary @hub-config.json

# Scheduled backups (SQLite snapshot + config), daily, keeping 7
iothub -db sqlite:/var/lib/iothub/readings.db -backup-dir /mnt/backup/iothub
curl -H "Authorization: Bearer $ADMIN" https://hub:8443/api/backups                      # files, last run, last error
curl -H "Authorization: Bearer $ADMIN" -X POST https://hub:8443/api/backups              # take one now
curl -H "Authorization: Bearer $ADMIN" -O https://hub:8443/api/backups/iothub-20261002-060000.db
```

**Import rules:**
- **All or nothing.** The whole file is checked before anything changes: every sensor definition and formula, the sensor limit, the dashboard JSON and the credentials. A bad file leaves the hub as it was.
- **Merge** adds and updates. **Replace** also deletes sensors (and devices, if the file has any) that are not in the file.
- **Unknown fields are rejected**, so a typo in a hand-edited file fails instead of being silently dropped.
- **Dry run.** `dryRun=1` returns the plan: created, updated and deleted sensors, and which devices would lose their sessions.
- **Device credentials** are exported only with `devices=1`. They are SHA-256 hashes of random 256-bit tokens, so they can't be turned back into tokens, but they do let whoever holds the file restore working access: keep the file private. Existing tokens keep working after a restore. A device whose token changes, or that a replace removes, is disconnected at once.

**Scheduled backups:**
- **Online.** The SQLite snapshot (`VACUUM INTO`) is consistent and compacted, and ingestion keeps running while it is taken.
- **Never half-written.** Files are written under a temporary name and renamed, so an interrupted backup never looks like a good one.
- **Private.** The directory is created `0700` and files `0600`, because the config file contains credential hashes.
- **Rotation.** The newest `-backup-keep` of each kind are kept.
- **Restarts don't postpone backups.** The schedule continues from the newest existing backup; an overdue one runs a minute after start. A failed backup is retried after at most 15 minutes and shows in `GET /api/backups`.
- **PostgreSQL:** the schedule saves the configuration only; use `pg_dump` (or your provider's backups) for the data.
- **Another disk.** A backup on the same disk as the database doesn't survive that disk failing. Point `-backup-dir` at another disk or a network share, or copy the files off.

**Restoring:**
1. Stop the hub.
2. Copy a `.db` snapshot over the database file. Remove any `-wal` and `-shm` files next to it.
3. Start the hub. Then `POST` the matching `.json` file to `/api/config?mode=replace`.

**Tested:**
- **Snapshot under load:** a snapshot of a 20 000-reading database while another goroutine kept writing. Writes continued during the snapshot; the copy passed `PRAGMA integrity_check` and had every reading.
- **Import:** all-or-nothing for seven kinds of bad file (store unchanged after each). Replace frees slots before adding, and a dry run changes nothing. Tokens survive a round trip; changed or removed devices are reported as revoked; two identities sharing a token are refused; a hub without `-auth-file` refuses credentials rather than losing them at restart.
- **Rotation and downloads:** files are rotated, with `0600`/`0700` modes. Path traversal (`../`, wrong names, `.partial` files) is refused. Viewers get 403.
- **Restore drill:** the built binary, 3000 readings → backup → data directory deleted → snapshot copied back and config imported. Same statistics (n 3000, mean 49.5) and the same sensor definition.

## Install as an app (PWA)

The dashboard can be installed on phones, tablets and PCs: it opens in its own window from the home screen or start menu, like a native app. There is nothing extra to deploy, because the hub serves the manifest, icons and service worker.

- **Android / Chrome / Edge:** the **Install** button in the header, or the browser's install icon.
- **iPhone / iPad:** Safari → Share → **Add to Home Screen**.
- **Requires HTTPS** (`-tls-cert`/`-tls-key`, or a TLS reverse proxy), or `localhost`. Browsers only install from a secure origin. On plain HTTP the dashboard still works as a web page.

**Offline behaviour, by design:**
- **The app shell is cached; data is not.** The service worker never caches `/api/` responses, so no plant data or signed-in content is stored on the device. An offline dashboard never shows old values as if they were live.
- **Opened without a connection,** it says so ("Offline", or "Hub unreachable" when the network is up but the hub isn't). It loads by itself when the hub answers: it retries with back-off up to every 60 s, and at once when the device reconnects.
- **Updates.** The shell is fetched network-first, so a hub upgrade reaches installed apps on their next online start.

**Serving:**
- **ETags.** Dashboard files carry a content-hash ETag and `Cache-Control: no-cache`, so an unchanged file costs a 304 on revisits instead of a full download.
- **Gzip.** Files are pre-compressed once at start and sent gzipped when the browser accepts it; `tiles.js`, for example, goes from 39 KB to 11.6 KB.

**Tested** in Chromium (Playwright, phone-sized viewport):
- **Installable:** Chrome reported no installability or manifest errors (`Page.getInstallabilityErrors`). The service worker controls the page and caches only the shell.
- **Offline:** launched offline, the shell opened with "Offline" and an explanation. Back online, the dashboard came up live without a reload. Zero API responses were in the cache.
- **Hub down:** reloaded while the hub was stopped, the page showed "Hub unreachable"; it went live by itself when the hub restarted.
- **Sign-in** still works with auth on.
- **Unit test:** gzip negotiation (including `q=0`), 304 revalidation, content types and 404s.
- **Not tested:** installing on real iOS and Android devices.

## Dashboard tiles

| Tile | Shows |
|---|---|
| `stat` | current value, threshold status, sparkline |
| `meter` | value against a min–max range with warn/critical colouring |
| `line` | up to 4 fields. **Live**, or a historical range (1 h – 30 d) drawn as bucket averages with a min–max band. Anomaly episodes appear as red markers, explained in the hover tooltip |
| `stats` | mean, min, max, std and sample count over 1 h – 30 d |
| `anomalies` | anomaly log for one sensor or all, open episodes first, live |
| `state` | text or on/off with a "normal" value |
| `eta` | time until a forecast reaches a limit, with range and a reliability label |

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

## Grafana and the full stack

[`deploy/`](deploy/) runs the hub with PostgreSQL and Grafana:

```bash
cd deploy && cp .env.example .env   # set every password; compose refuses to start without them
docker compose up -d                # hub :8080 / MQTT :1883, Grafana :3000
```

- **Least-privilege database roles** ([`postgres/01-roles.sh`](deploy/postgres/01-roles.sh), run once on first start):
  - the superuser is used only for initialisation
  - the hub connects as `iothub`, which owns its tables but is not a superuser
  - Grafana connects as `grafana_ro`: SELECT only, read-only transactions, 30 s statement timeout
- **Provisioned data source and dashboard** ("IoT Hub — overview"), picking a sensor and field:
  - summary stats (mean, min, max, std, samples) for the time range
  - average with a min–max band from the 1-minute rollups, with anomaly episodes as annotations
  - anomalies by sensor, the anomaly log, and the text-state change log (e.g. door open/close)
  - an active-anomaly count
- **Roles of the two dashboards.** Use Grafana for ad-hoc analysis, comparisons and reports. The hub's own dashboard stays the live operations view (sub-second updates, forecasts, writes).

**Tested here:**
- **Roles script:** run as superuser before any tables existed, as Docker's init does. The hub then created its tables as `iothub`.
- **Queries:** every dashboard query ran as `grafana_ro` against 24 h of hub-written data, with Grafana's macros (`$__unixEpochFrom/To`, `$__interval_ms`, variables) expanded:
  - correct values: rollup stats match the data; a 10-minute interval gives 145 buckets with the injected spike as the bucket maximum
- **Write protection:** writes are refused by `grafana_ro` twice over. With read-only transactions switched off, table and schema privileges still deny INSERT, DELETE, CREATE and DROP.
- **Compose:** `docker compose config` validates.
- **Not tested:** Grafana itself (it couldn't be downloaded here) and a real `docker compose up` (no Docker daemon). The panel JSON follows Grafana's schema 39, but treat the first start as the check, and adjust panel options in the UI if a field doesn't render as intended.

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

- **No per-IP rate limiting or lockout.** Tokens are 256-bit, so guessing is not feasible, but noisy scanners are not throttled. Put the hub behind a firewall or reverse proxy if it faces the internet.
- **Device credentials are tokens, not client certificates.** Mutual TLS would bind identity to hardware keys; the listener supports it, but it isn't wired up.
- **Anomalies are statistical, not semantic.** The detector knows "unusual for this field", not "bad for this machine". Use `range` rules for known limits, and models (see ML.NET integration) for multi-field judgements.
- **Statistics are mean/std/min/max.** No percentiles: they don't merge across rollups, so they would need raw scans or sketches.
- **The embedded broker is a single node.** To use an existing broker (Mosquitto, EMQX), run with `-mqtt ""` and add a small subscriber that calls `Pipeline.Handle`.
- **Deleting a sensor keeps its database history.** Re-creating the same id continues the series.
