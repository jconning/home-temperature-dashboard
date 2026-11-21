package main

import (
	"bufio"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseGoveeCSV reads a history export from the Govee Home app (Device ->
// Settings -> Data export). Columns are found by header name, so both the
// Fahrenheit and Celsius variants work, e.g.
//
//	Timestamp for sample frequency every 1 min min,Temperature_Fahrenheit,Relative_Humidity
//	2024-01-15 10:23:00,71.2,48.1
//
// Timestamps carry no zone and are interpreted in loc.
//
// Exports reflect the app's calibration and say so in the header, e.g.
// "Temperature_Fahrenheit(calibrated -0.5 deg F)". Those offsets are returned
// so raw Bluetooth readings can be adjusted to match.
func ParseGoveeCSV(r io.Reader, deviceID string, loc *time.Location) ([]Reading, Calibration, error) {
	var cal Calibration
	rs, err := parseGoveeCSV(r, deviceID, loc, &cal)
	return rs, cal, err
}

// Calibration holds offsets found in an export header; nil means not stated.
type Calibration struct {
	TempF *float64
	Hum   *float64
}

var calibratedRE = regexp.MustCompile(`(?i)calibrated\s*([-+]?\d+(?:\.\d+)?)`)

func headerCalibration(h string) *float64 {
	m := calibratedRE.FindStringSubmatch(h)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

func parseGoveeCSV(r io.Reader, deviceID string, loc *time.Location, cal *Calibration) ([]Reading, error) {
	br := bufio.NewReader(r)
	// Strip a UTF-8 BOM if present.
	if b, _ := br.Peek(3); len(b) == 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	tsCol, tempCol, humCol := -1, -1, -1
	celsius := false
	for i, h := range header {
		l := strings.ToLower(h)
		switch {
		case tsCol < 0 && (strings.Contains(l, "time") || strings.Contains(l, "date")):
			tsCol = i
		case tempCol < 0 && strings.Contains(l, "temp"):
			tempCol = i
			celsius = strings.Contains(l, "celsius") || strings.Contains(l, "℃") || strings.Contains(l, "(c)")
			if v := headerCalibration(h); v != nil {
				f := *v
				if celsius || strings.Contains(l, "deg c") {
					f *= 9.0 / 5 // a temperature difference, so no +32
				}
				cal.TempF = &f
			}
		case humCol < 0 && strings.Contains(l, "humid"):
			humCol = i
			cal.Hum = headerCalibration(h)
		}
	}
	if tsCol < 0 || tempCol < 0 {
		return nil, fmt.Errorf("could not find timestamp and temperature columns in header %q", strings.Join(header, ","))
	}

	layouts := []string{
		"2006-01-02 15:04:05", "2006-01-02 15:04", "2006/01/02 15:04:05", "2006/01/02 15:04",
		"1/2/2006 15:04:05", "1/2/2006 15:04", "01-02-2006 15:04:05", time.RFC3339,
	}
	var out []Reading
	line := 1
	for {
		rec, err := cr.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(rec) <= tsCol || len(rec) <= tempCol || strings.TrimSpace(rec[tsCol]) == "" {
			continue
		}
		var ts time.Time
		var perr error
		for _, layout := range layouts {
			if ts, perr = time.ParseInLocation(layout, strings.TrimSpace(rec[tsCol]), loc); perr == nil {
				break
			}
		}
		if perr != nil {
			return nil, fmt.Errorf("line %d: unrecognised timestamp %q", line, rec[tsCol])
		}
		temp, err := strconv.ParseFloat(strings.TrimSpace(rec[tempCol]), 64)
		if err != nil {
			continue
		}
		if celsius {
			temp = temp*9/5 + 32
		}
		r := Reading{DeviceID: deviceID, TS: ts.UTC(), TempF: temp, Source: "import"}
		if humCol >= 0 && humCol < len(rec) {
			if h, err := strconv.ParseFloat(strings.TrimSpace(rec[humCol]), 64); err == nil {
				r.Humidity = sql.NullFloat64{Float64: h, Valid: true}
			}
		}
		out = append(out, r)
	}
	return out, nil
}
