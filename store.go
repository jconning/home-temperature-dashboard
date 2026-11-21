package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// Store is the MySQL cache of devices and readings. All timestamps are UTC.
type Store struct {
	db *sql.DB
}

type Device struct {
	ID          string
	SKU         string
	Name        string
	Slot        int     // 1-based categorical color slot, stable by name order
	TempOffsetF float64 // Govee app calibration; raw Bluetooth readings need it added
	HumOffset   float64
	BLEAddress  string // this computer's Bluetooth identifier for the sensor, once seen
}

type Reading struct {
	DeviceID string
	TS       time.Time
	TempF    float64
	Humidity sql.NullFloat64
	Source   string
}

// Bucket is one aggregated time bucket of readings for a device.
type Bucket struct {
	T                      time.Time
	TempAvg, TempMin, TempMax float64
	Hum                    sql.NullFloat64
}

type Stats struct {
	Count                     int
	TempMin, TempMax, TempAvg float64
	HumMin, HumMax, HumAvg    sql.NullFloat64
	First, Last               time.Time
}

func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to mysql: %w", err)
	}
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate brings tables created by older versions up to schema.sql.
func (s *Store) migrate(ctx context.Context) error {
	hasColumn := func(table, column string) (bool, error) {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, column).Scan(&n)
		return n > 0, err
	}
	for _, m := range []struct{ column, ddl string }{
		{"temp_offset_f", `ALTER TABLE devices ADD COLUMN temp_offset_f DECIMAL(5,2) NOT NULL DEFAULT 0 AFTER name`},
		{"hum_offset", `ALTER TABLE devices ADD COLUMN hum_offset DECIMAL(4,1) NOT NULL DEFAULT 0 AFTER temp_offset_f`},
		{"ble_address", `ALTER TABLE devices ADD COLUMN ble_address VARCHAR(64) NULL AFTER hum_offset`},
	} {
		ok, err := hasColumn("devices", m.column)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := s.db.ExecContext(ctx, m.ddl); err != nil {
				return err
			}
		}
	}
	var colType string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COLUMN_TYPE FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'readings' AND COLUMN_NAME = 'source'`).Scan(&colType); err != nil {
		return err
	}
	if !strings.Contains(colType, "'ble'") {
		// Appending an ENUM value is a metadata-only change.
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE readings MODIFY source ENUM('api','import','ble') NOT NULL DEFAULT 'api'`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertDevice(ctx context.Context, id, sku, name string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (device_id, sku, name) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE sku = VALUES(sku), name = VALUES(name)`,
		id, sku, name)
	return err
}

func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT device_id, sku, name, temp_offset_f, hum_offset, COALESCE(ble_address, '')
		   FROM devices ORDER BY name, device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.SKU, &d.Name, &d.TempOffsetF, &d.HumOffset, &d.BLEAddress); err != nil {
			return nil, err
		}
		d.Slot = len(out) + 1
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetBLEAddress(ctx context.Context, id, addr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET ble_address = ? WHERE device_id = ?`, addr, id)
	return err
}

// SetCalibration records the Govee app's calibration offsets for a device;
// nil leaves a value unchanged.
func (s *Store) SetCalibration(ctx context.Context, id string, tempF, hum *float64) error {
	if tempF != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE devices SET temp_offset_f = ? WHERE device_id = ?`, *tempF, id); err != nil {
			return err
		}
	}
	if hum != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE devices SET hum_offset = ? WHERE device_id = ?`, *hum, id); err != nil {
			return err
		}
	}
	return nil
}

// LatestDetailed returns the newest per-minute reading (Bluetooth or CSV
// import) for a device; API snapshots don't count. Zero if none.
func (s *Store) LatestDetailed(ctx context.Context, id string) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(ts) FROM readings WHERE device_id = ? AND source IN ('ble', 'import')`, id).Scan(&t)
	return t.Time, err
}

// DeleteAPIBetween drops API snapshots in [from, to], once per-minute data
// covers that span.
func (s *Store) DeleteAPIBetween(ctx context.Context, id string, from, to time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM readings WHERE device_id = ? AND source = 'api' AND ts BETWEEN ? AND ?`,
		id, from.UTC(), to.UTC())
	return err
}

func (s *Store) InsertReading(ctx context.Context, r Reading) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO readings (device_id, ts, temp_f, humidity, source) VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE temp_f = VALUES(temp_f), humidity = VALUES(humidity), source = VALUES(source)`,
		r.DeviceID, r.TS.UTC(), r.TempF, r.Humidity, r.Source)
	return err
}

// InsertReadingsIgnore bulk-inserts readings, skipping any (device, ts) already
// cached. It returns how many rows were new.
func (s *Store) InsertReadingsIgnore(ctx context.Context, rs []Reading) (int64, error) {
	const chunk = 500
	var total int64
	for i := 0; i < len(rs); i += chunk {
		part := rs[i:min(i+chunk, len(rs))]
		var sb strings.Builder
		sb.WriteString(`INSERT IGNORE INTO readings (device_id, ts, temp_f, humidity, source) VALUES `)
		args := make([]any, 0, len(part)*5)
		for j, r := range part {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?)")
			args = append(args, r.DeviceID, r.TS.UTC(), r.TempF, r.Humidity, r.Source)
		}
		res, err := s.db.ExecContext(ctx, sb.String(), args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Latest returns the most recent reading per device.
func (s *Store) Latest(ctx context.Context) (map[string]Reading, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.device_id, r.ts, r.temp_f, r.humidity, r.source
		   FROM readings r
		   JOIN (SELECT device_id, MAX(ts) AS ts FROM readings GROUP BY device_id) m
		     ON m.device_id = r.device_id AND m.ts = r.ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Reading{}
	for rows.Next() {
		var r Reading
		if err := rows.Scan(&r.DeviceID, &r.TS, &r.TempF, &r.Humidity, &r.Source); err != nil {
			return nil, err
		}
		out[r.DeviceID] = r
	}
	return out, rows.Err()
}

// Series aggregates readings in [from, to) into fixed-width buckets, keyed by
// device. deviceID == "" means all devices.
func (s *Store) Series(ctx context.Context, from, to time.Time, bucket time.Duration, deviceID string) (map[string][]Bucket, error) {
	sec := int64(bucket / time.Second)
	if sec < 1 {
		sec = 1
	}
	q := `SELECT device_id, CAST(UNIX_TIMESTAMP(ts) DIV ? AS SIGNED) * ? AS b,
	             AVG(temp_f), MIN(temp_f), MAX(temp_f), AVG(humidity)
	        FROM readings
	       WHERE ts >= ? AND ts < ?`
	args := []any{sec, sec, from.UTC(), to.UTC()}
	if deviceID != "" {
		q += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	q += ` GROUP BY device_id, b ORDER BY device_id, b`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Bucket{}
	for rows.Next() {
		var id string
		var b int64
		var bk Bucket
		if err := rows.Scan(&id, &b, &bk.TempAvg, &bk.TempMin, &bk.TempMax, &bk.Hum); err != nil {
			return nil, err
		}
		// Plot each bucket at its midpoint.
		bk.T = time.Unix(b+sec/2, 0).UTC()
		out[id] = append(out[id], bk)
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context, from, to time.Time, deviceID string) (Stats, error) {
	var st Stats
	var first, last sql.NullTime
	var tmin, tmax, tavg sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(temp_f), MAX(temp_f), AVG(temp_f),
		        MIN(humidity), MAX(humidity), AVG(humidity), MIN(ts), MAX(ts)
		   FROM readings WHERE device_id = ? AND ts >= ? AND ts < ?`,
		deviceID, from.UTC(), to.UTC()).
		Scan(&st.Count, &tmin, &tmax, &tavg, &st.HumMin, &st.HumMax, &st.HumAvg, &first, &last)
	st.TempMin, st.TempMax, st.TempAvg = tmin.Float64, tmax.Float64, tavg.Float64
	st.First, st.Last = first.Time, last.Time
	return st, err
}

// Earliest returns the oldest cached reading time (zero if none).
func (s *Store) Earliest(ctx context.Context) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM readings`).Scan(&t)
	return t.Time, err
}

func (s *Store) TotalReadings(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM readings`).Scan(&n)
	return n, err
}

func (s *Store) LogPoll(ctx context.Context, started time.Time, dur time.Duration, ok, failed int, errText string) error {
	var e sql.NullString
	if errText != "" {
		e = sql.NullString{String: errText, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_log (started_at, duration_ms, devices_ok, devices_failed, error) VALUES (?, ?, ?, ?, ?)`,
		started.UTC(), dur.Milliseconds(), ok, failed, e)
	return err
}
