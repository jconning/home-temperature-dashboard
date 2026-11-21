# home-temperature-dashboard

A Go web app that collects readings from Govee H5179 thermo-hygrometers, caches
them in MySQL and serves server-rendered temperature and humidity charts with an
HVAC-technician theme ("HVAC Thermal Survey"). The program and its macOS login
item are named `govee-hvac`.

**[View the explainer page](https://jconning.github.io/home-temperature-dashboard/explainer/)**:
an animated walkthrough of how it all works, with screenshots (source in
`docs/explainer/`, served by GitHub Pages from `/docs`).
Device IDs and Bluetooth names in the docs and tests are placeholders such as
`A1:B2:C3:D4:E5:F6:1A:2B` / `GV5179_1A2B`; the app learns the real ones from the
Govee API.

## How it works

- **Bluetooth history (the main source):** Each H5179 logs a reading every minute and
  keeps the last **20 days** (28,800 minutes) on the sensor. Every `-ble-every`
  (default 3 h, and once at startup) the app connects to each sensor over Bluetooth.
  It downloads every minute since the newest per-minute reading it already has, and
  stores them as `source='ble'`. This is the same request the Govee app's "Get All
  Data" makes. The computer only has to be on and within Bluetooth range at least once
  every 20 days. Both schedules check the wall clock each minute (`schedule.go`), so a
  sync that came due while the Mac slept runs within a minute of waking. The protocol
  is in `goveeble.go` (encrypted handshake, then the history request) and the
  collector is in `ble.go`. It is based on
  [GoveeBTTempLogger](https://github.com/wcbonner/GoveeBTTempLogger) and was verified
  against these sensors.
  - Raw sensor readings ignore the Govee app's calibration. The app adds each sensor's
    `devices.temp_offset_f` / `hum_offset`, which it reads from CSV export headers such
    as `calibrated -0.5 deg F`.
  - macOS identifies each sensor by a per-computer address, which is remembered in
    `devices.ble_address` so later syncs connect without scanning.
  - Terminal (or whatever runs the app) needs Bluetooth permission in System Settings →
    Privacy & Security → Bluetooth.
- **Cloud polling (rough "now" value):** The Govee OpenAPI
  (`openapi.api.govee.com/router/api/v1`) only returns a device's *current* state, and
  that value refreshes only about hourly. The app syncs the device list at startup and
  every 6 h. It polls `/device/state` every `-poll` (default 15 min) to show a recent
  value between Bluetooth syncs. API snapshots are deleted once per-minute data covers
  their time. Readings taken while a sensor reports offline are skipped, because Govee
  says those values are stale.
- **Cache:** The `readings` table (`device_id, ts` UTC, `temp_f`, `humidity`) is keyed
  per device and second. `poll_log` records each poll cycle. The tables are created on
  startup (`schema.sql`).
- **Back-fill beyond 20 days:** On `/import` you can upload the CSV files that the Govee
  Home app exports. Govee keeps longer history in its cloud and splits it into
  50,000-minute files. Rows already cached are skipped, °C files are converted to °F,
  and the calibration in the header is saved for Bluetooth readings.
- **Charts:** The charts are SVG generated in Go (`chart.go`) and work with no JavaScript.
  `static/app.js` only adds the hover crosshair and tooltip. Long ranges are averaged
  into about 400 buckets in SQL. Lines break where no data was logged.

## Where it can run

The per-minute history comes over Bluetooth, which only reaches about 10 m, so
the app has to run on a computer **inside the house, within Bluetooth range of
the sensors** (being on the home network is not enough). In its current form it
is not suitable for cloud hosting. A cloud copy with `-ble=false` would only get
the Govee API's current value, which refreshes about hourly, plus whatever CSV
exports you upload. The computer doesn't have to be on all the time: anything it
misses is downloaded on the next sync, as long as that is within 20 days.

## Run

```sh
go build -o govee-hvac . && ./govee-hvac          # http://localhost:8179
./govee-hvac -ble-every 1h -poll 30m -tz America/Chicago
./govee-hvac -ble=false                            # no Bluetooth (e.g. on a server)
```

### Start at login

The app runs as the LaunchAgent `com.jimconning.govee-hvac`. It starts at login, restarts
if it exits, and logs to `~/Library/Logs/govee-hvac.log`. At startup it waits up to two
minutes for MySQL, which also starts at login.

```sh
zsh scripts/install-launchagent.sh                              # build + (re)install + start
launchctl kickstart -k gui/$(id -u)/com.jimconning.govee-hvac   # restart
launchctl bootout gui/$(id -u)/com.jimconning.govee-hvac        # stop and unload
```

Use `scripts/build.sh`, not plain `go build`. It embeds `packaging/Info.plist`, whose
Bluetooth usage description macOS requires for a program that isn't run from a
terminal. Each rebuild changes the ad-hoc signature, so macOS may ask for Bluetooth
permission again. Don't also start `./govee-hvac` by hand while the agent is running,
because both want port 8179.

The secrets live in `config/` (gitignored, mode 600). You can override them with
environment variables.

| File                     | Env var         | Contents                    |
|--------------------------|-----------------|-----------------------------|
| `config/govee_api_key`   | `GOVEE_API_KEY` | Govee API key               |
| `config/db_dsn`          | `GOVEE_DSN`     | MySQL DSN for user `govee`  |
| `config/db_password`     |                 | password used to build the DSN |

## Database

Database `govee_thermo`. User `'govee'@'%'` has `ALL PRIVILEGES` on `govee_thermo.*`
only. To recreate it:

```sh
zsh scripts/setup_db.sh                   # writes config/db_setup.sql + config/db_dsn
mysql -u root -p < config/db_setup.sql    # as a MySQL admin
```

Note that MySQL may only listen on 127.0.0.1. In that case the `%` host lets `govee`
connect from other machines only after `bind-address` is opened up.

## Test

```sh
go test ./...
```

## License

MIT; see [LICENSE](LICENSE). The Bluetooth history protocol follows
[GoveeBTTempLogger](https://github.com/wcbonner/GoveeBTTempLogger) by William C
Bonner (MIT); its notice is in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
