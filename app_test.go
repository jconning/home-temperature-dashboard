package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseGoveeCSVFahrenheit(t *testing.T) {
	in := "\xef\xbb\xbfTimestamp for sample frequency every 1 min min,Temperature_Fahrenheit,Relative_Humidity\n" +
		"2024-01-15 10:23:00,71.2,48.1\n" +
		"2024-01-15 10:24:00,71.3,48.0\n"
	loc := time.FixedZone("CST", -6*3600)
	rs, cal, err := ParseGoveeCSV(strings.NewReader(in), "dev1", loc)
	if err != nil {
		t.Fatal(err)
	}
	if cal.TempF != nil || cal.Hum != nil {
		t.Errorf("no calibration in header, got %+v", cal)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d rows", len(rs))
	}
	if want := time.Date(2024, 1, 15, 16, 23, 0, 0, time.UTC); !rs[0].TS.Equal(want) {
		t.Errorf("ts = %v, want %v", rs[0].TS, want)
	}
	if rs[0].TempF != 71.2 || !rs[0].Humidity.Valid || rs[0].Humidity.Float64 != 48.1 || rs[0].Source != "import" {
		t.Errorf("row = %+v", rs[0])
	}
}

func TestParseGoveeCSVCelsius(t *testing.T) {
	in := "Time,Temperature_Celsius(℃),Relative_Humidity(%)\n2024/03/01 08:00,20,40\n"
	rs, _, err := ParseGoveeCSV(strings.NewReader(in), "dev1", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].TempF != 68 {
		t.Fatalf("got %+v", rs)
	}
}

func TestParseGoveeCSVCalibration(t *testing.T) {
	// Header exactly as in the user's Downstairs exports.
	in := "\xef\xbb\xbfTimestamp for sample frequency every 1 min min,Temperature_Fahrenheit(calibrated -0.5 deg F),Relative_Humidity\n" +
		"2026-11-20 17:20:00,71.46,53.6\n"
	rs, cal, err := ParseGoveeCSV(strings.NewReader(in), "d", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].TempF != 71.46 {
		t.Fatalf("rows %+v", rs)
	}
	if cal.TempF == nil || *cal.TempF != -0.5 || cal.Hum != nil {
		t.Fatalf("calibration %+v", cal)
	}
	in = "Time,Temperature_Celsius(calibrated 1 deg C),Relative_Humidity(calibrated -2.0)\n2024/03/01 08:00,20,40\n"
	if _, cal, _ = ParseGoveeCSV(strings.NewReader(in), "d", time.UTC); cal.TempF == nil || *cal.TempF != 1.8 || cal.Hum == nil || *cal.Hum != -2 {
		t.Fatalf("celsius calibration %+v", cal)
	}
}

func TestParseGoveeCSVBadHeader(t *testing.T) {
	if _, _, err := ParseGoveeCSV(strings.NewReader("a,b\n1,2\n"), "d", time.UTC); err == nil {
		t.Fatal("expected error")
	}
}

func TestRenderChartBreaksGaps(t *testing.T) {
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	pts := []ChartPoint{
		{T: base, V: 70}, {T: base.Add(2 * time.Minute), V: 71},
		{T: base.Add(3 * time.Hour), V: 72}, {T: base.Add(3*time.Hour + 2*time.Minute), V: 73},
	}
	html := string(RenderChart([]ChartSeries{{Name: "Den", Slot: 1, Points: pts}}, ChartOpts{
		ID: "t", Unit: "°F", From: base, To: base.Add(4 * time.Hour), Gap: 15 * time.Minute,
		Loc: time.UTC, MinSpan: 4, Decimals: 1,
	}))
	if n := strings.Count(html, `class="line s1"`); n != 2 {
		t.Errorf("want 2 line segments, got %d", n)
	}
	if !strings.Contains(html, "Den 73.0°F") {
		t.Error("missing direct end label")
	}
}

func TestRenderChartEmpty(t *testing.T) {
	html := string(RenderChart(nil, ChartOpts{From: time.Now().Add(-time.Hour), To: time.Now(), Loc: time.UTC}))
	if !strings.Contains(html, "chart-empty") {
		t.Error("expected empty state")
	}
}

func TestNiceStep(t *testing.T) {
	for in, want := range map[float64]float64{0.8: 1, 1.3: 2, 2.2: 2.5, 4: 5, 7: 10, 0.3: 0.5} {
		if got := niceStep(in); got != want {
			t.Errorf("niceStep(%v) = %v, want %v", in, got, want)
		}
	}
}
