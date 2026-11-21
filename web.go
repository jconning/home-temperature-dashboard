package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const staleAfter = 30 * time.Minute

type rangeOpt struct {
	Key, Label string
	Dur        time.Duration // 0 = all data
}

var ranges = []rangeOpt{
	{"6h", "6 H", 6 * time.Hour},
	{"24h", "24 H", 24 * time.Hour},
	{"3d", "3 D", 3 * 24 * time.Hour},
	{"7d", "7 D", 7 * 24 * time.Hour},
	{"30d", "30 D", 30 * 24 * time.Hour},
	{"90d", "90 D", 90 * 24 * time.Hour},
	{"1y", "1 Y", 365 * 24 * time.Hour},
	{"all", "ALL", 0},
}

type Server struct {
	store  *Store
	poller *Poller
	ble    *BLECollector // nil when Bluetooth collection is off
	loc    *time.Location
	pages  map[string]*template.Template
}

type roomCard struct {
	Device
	Zone        int
	HasReading  bool
	TempF       float64
	Hum         float64
	HasHum      bool
	When        time.Time
	Status      string // online | stale | offline | nodata
	StatusLabel string
	Angle       float64 // gauge needle angle, degrees
}

type tableData struct {
	Headers []string
	Rows    [][]string
}

type pageData struct {
	Title      string
	Nav        string
	Range      rangeOpt
	Ranges     []rangeOpt
	RangePath  string
	Rooms      []roomCard
	Room       *roomCard
	Stats      Stats
	TempChart  template.HTML
	HumChart   template.HTML
	Legend     []Device
	Table      tableData
	Poll       PollStatus
	BLE        *BLEStatus
	Interval   time.Duration
	Total      int64
	Msg        string
	Err        string
	Now        time.Time
	BucketDesc string
}

func NewServer(store *Store, poller *Poller, ble *BLECollector, loc *time.Location) (*Server, error) {
	funcs := template.FuncMap{
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc).Format("Jan 2, 3:04 PM")
		},
		"ago":  humanAgo,
		"path": url.PathEscape,
		"f1":   func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"pct":  func(v float64) string { return fmt.Sprintf("%.0f", math.Max(0, math.Min(100, v))) },
		"sub":  func(a, b float64) float64 { return a - b },
		"gaugeFace": func() template.HTML { return gaugeFaceSVG },
	}
	s := &Server{store: store, poller: poller, ble: ble, loc: loc, pages: map[string]*template.Template{}}
	for _, p := range []string{"dashboard", "room", "import"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}
	return s, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /room/{id}", s.room)
	mux.HandleFunc("POST /poll", s.pollNow)
	mux.HandleFunc("POST /ble-sync", s.bleSyncNow)
	mux.HandleFunc("GET /import", s.importForm)
	mux.HandleFunc("POST /import", s.importUpload)
	return logRequests(mux)
}

func (s *Server) render(w http.ResponseWriter, page string, d *pageData) {
	d.Ranges = ranges
	d.Poll = s.poller.Status()
	if s.ble != nil {
		st := s.ble.Status()
		d.BLE = &st
	}
	d.Interval = s.poller.interval
	d.Now = time.Now()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout.html", d); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "Service fault: "+err.Error(), http.StatusInternalServerError)
}

func pickRange(r *http.Request) rangeOpt {
	key := r.URL.Query().Get("range")
	for _, ro := range ranges {
		if ro.Key == key {
			return ro
		}
	}
	return ranges[0]
}

// window resolves the time window and an aggregation bucket that keeps each
// series to roughly 400 points.
func (s *Server) window(r *http.Request, ro rangeOpt) (from, to time.Time, bucket time.Duration, err error) {
	to = time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if ro.Dur > 0 {
		from = to.Add(-ro.Dur)
	} else {
		earliest, e := s.store.Earliest(r.Context())
		if e != nil {
			return from, to, 0, e
		}
		from = earliest
		if from.IsZero() || to.Sub(from) < 24*time.Hour {
			from = to.Add(-24 * time.Hour)
		}
	}
	bucket = (to.Sub(from) / 400).Truncate(time.Minute)
	if bucket < time.Minute {
		bucket = time.Minute
	}
	return from, to, bucket, nil
}

func (s *Server) gap(bucket time.Duration) time.Duration {
	return max(3*bucket, 4*s.poller.interval, 15*time.Minute)
}

func (s *Server) roomCards(r *http.Request, devs []Device) ([]roomCard, error) {
	latest, err := s.store.Latest(r.Context())
	if err != nil {
		return nil, err
	}
	online := s.poller.Status().Online
	cards := make([]roomCard, 0, len(devs))
	for i, d := range devs {
		c := roomCard{Device: d, Zone: i + 1}
		lr, ok := latest[d.ID]
		if ok {
			c.HasReading = true
			c.TempF = lr.TempF
			c.Hum, c.HasHum = lr.Humidity.Float64, lr.Humidity.Valid
			c.When = lr.TS
			// Gauge face spans 40–100 °F over -120°..+120°.
			c.Angle = math.Max(-120, math.Min(120, (lr.TempF-70)*4))
		}
		on, known := online[d.ID]
		switch {
		case known && !on:
			c.Status, c.StatusLabel = "offline", "Offline"
		case !ok:
			c.Status, c.StatusLabel = "nodata", "Awaiting data"
		case time.Since(lr.TS) > staleAfter:
			c.Status, c.StatusLabel = "stale", "Stale"
		default:
			c.Status, c.StatusLabel = "online", "Online"
		}
		cards = append(cards, c)
	}
	return cards, nil
}

func (s *Server) buildCharts(d *pageData, devs []Device, series map[string][]Bucket, from, to time.Time, bucket time.Duration, band bool, idPrefix string) {
	var temps, hums []ChartSeries
	for _, dev := range devs {
		bs := series[dev.ID]
		ts := ChartSeries{Name: dev.Name, Slot: dev.Slot}
		hs := ChartSeries{Name: dev.Name, Slot: dev.Slot}
		for _, b := range bs {
			ts.Points = append(ts.Points, ChartPoint{T: b.T, V: b.TempAvg, Lo: b.TempMin, Hi: b.TempMax})
			if b.Hum.Valid {
				hs.Points = append(hs.Points, ChartPoint{T: b.T, V: b.Hum.Float64})
			}
		}
		temps = append(temps, ts)
		hums = append(hums, hs)
	}
	gap := s.gap(bucket)
	d.TempChart = RenderChart(temps, ChartOpts{ID: idPrefix + "temp", Label: "Temperature over time, °F", Unit: "°F",
		From: from, To: to, Gap: gap, Loc: s.loc, Band: band, MinSpan: 4, Decimals: 1})
	d.HumChart = RenderChart(hums, ChartOpts{ID: idPrefix + "hum", Label: "Relative humidity over time, %", Unit: "%",
		From: from, To: to, Gap: gap, Loc: s.loc, MinSpan: 10, Decimals: 1})
	d.Legend = devs
	d.BucketDesc = describeBucket(bucket)
	d.Table = s.buildTable(devs, series)
}

// buildTable is the accessible data view: one row per bucket, newest first.
func (s *Server) buildTable(devs []Device, series map[string][]Bucket) tableData {
	td := tableData{Headers: []string{"Time"}}
	byTime := map[int64][]string{}
	for i, dev := range devs {
		td.Headers = append(td.Headers, dev.Name+" °F", dev.Name+" %RH")
		for _, b := range series[dev.ID] {
			row, ok := byTime[b.T.Unix()]
			if !ok {
				row = make([]string, 1+2*len(devs))
				row[0] = b.T.In(s.loc).Format("Jan 2 3:04 PM")
				byTime[b.T.Unix()] = row
			}
			row[1+2*i] = fmt.Sprintf("%.1f", b.TempAvg)
			if b.Hum.Valid {
				row[2+2*i] = fmt.Sprintf("%.1f", b.Hum.Float64)
			}
		}
	}
	keys := make([]int64, 0, len(byTime))
	for k := range byTime {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	for _, k := range keys {
		td.Rows = append(td.Rows, byTime[k])
	}
	return td
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ro := pickRange(r)
	devs, err := s.store.Devices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cards, err := s.roomCards(r, devs)
	if err != nil {
		s.fail(w, err)
		return
	}
	from, to, bucket, err := s.window(r, ro)
	if err != nil {
		s.fail(w, err)
		return
	}
	series, err := s.store.Series(ctx, from, to, bucket, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	total, _ := s.store.TotalReadings(ctx)
	d := &pageData{Title: "Thermal Survey", Nav: "dashboard", Range: ro, RangePath: "/", Rooms: cards, Total: total,
		Msg: r.URL.Query().Get("msg")}
	s.buildCharts(d, devs, series, from, to, bucket, false, "")
	s.render(w, "dashboard", d)
}

func (s *Server) room(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	ro := pickRange(r)
	devs, err := s.store.Devices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cards, err := s.roomCards(r, devs)
	if err != nil {
		s.fail(w, err)
		return
	}
	var card *roomCard
	for i := range cards {
		if cards[i].ID == id {
			card = &cards[i]
		}
	}
	if card == nil {
		http.NotFound(w, r)
		return
	}
	from, to, bucket, err := s.window(r, ro)
	if err != nil {
		s.fail(w, err)
		return
	}
	series, err := s.store.Series(ctx, from, to, bucket, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	stats, err := s.store.Stats(ctx, from, to, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := &pageData{Title: card.Name, Nav: "room", Range: ro, RangePath: "/room/" + url.PathEscape(id),
		Rooms: cards, Room: card, Stats: stats}
	s.buildCharts(d, []Device{card.Device}, series, from, to, bucket, true, "room-")
	s.render(w, "room", d)
}

func (s *Server) pollNow(w http.ResponseWriter, r *http.Request) {
	s.poller.TriggerNow()
	redirectBack(w, r)
}

func (s *Server) bleSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.ble != nil {
		s.ble.TriggerNow()
	}
	redirectBack(w, r)
}

func redirectBack(w http.ResponseWriter, r *http.Request) {
	back := r.Referer()
	if u, err := url.Parse(back); err != nil || back == "" || u.Host != r.Host {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) importForm(w http.ResponseWriter, r *http.Request) {
	devs, err := s.store.Devices(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	cards, err := s.roomCards(r, devs)
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	s.render(w, "import", &pageData{Title: "Import History", Nav: "import", Rooms: cards, Msg: q.Get("msg"), Err: q.Get("err")})
}

func (s *Server) importUpload(w http.ResponseWriter, r *http.Request) {
	redirect := func(key, msg string) {
		http.Redirect(w, r, "/import?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		redirect("err", "Upload failed: "+err.Error())
		return
	}
	id := r.FormValue("device")
	devs, err := s.store.Devices(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var dev *Device
	for i := range devs {
		if devs[i].ID == id {
			dev = &devs[i]
		}
	}
	if dev == nil {
		redirect("err", "Pick which room the file belongs to.")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		redirect("err", "Choose a CSV file to upload.")
		return
	}
	defer f.Close()
	rs, cal, err := ParseGoveeCSV(f, dev.ID, s.loc)
	if err != nil {
		redirect("err", "Could not read CSV: "+err.Error())
		return
	}
	n, err := s.store.InsertReadingsIgnore(r.Context(), rs)
	if err != nil {
		redirect("err", "Database error: "+err.Error())
		return
	}
	if len(rs) > 0 {
		first, last := rs[0].TS, rs[0].TS
		for _, x := range rs {
			first, last = minTime(first, x.TS), maxTime(last, x.TS)
		}
		// Per-minute data now covers this span; drop the coarse API snapshots in it.
		if err := s.store.DeleteAPIBetween(r.Context(), dev.ID, first, last); err != nil {
			log.Printf("import: prune API readings: %v", err)
		}
	}
	msg := fmt.Sprintf("%s: read %d rows, cached %d new readings (%d already on file).", dev.Name, len(rs), n, int64(len(rs))-n)
	if cal.TempF != nil || cal.Hum != nil {
		if err := s.store.SetCalibration(r.Context(), dev.ID, cal.TempF, cal.Hum); err != nil {
			redirect("err", "Database error: "+err.Error())
			return
		}
		if cal.TempF != nil {
			msg += fmt.Sprintf(" Calibration %+.2f °F noted for Bluetooth readings.", *cal.TempF)
		}
	}
	redirect("msg", msg)
}

// gaugeFaceSVG is the static part of the manifold-style dial: cold/comfort/hot
// zone arcs, tick marks and labels. 40–100 °F spans -120°..+120°.
var gaugeFaceSVG = func() template.HTML {
	const cx, cy = 100.0, 110.0
	pt := func(f, r float64) (float64, float64) {
		a := (f - 70) * 4 * math.Pi / 180
		return cx + r*math.Sin(a), cy - r*math.Cos(a)
	}
	arc := func(class string, f0, f1 float64) string {
		x0, y0 := pt(f0, 76)
		x1, y1 := pt(f1, 76)
		large := 0
		if (f1-f0)*4 > 180 {
			large = 1
		}
		return fmt.Sprintf(`<path class="zone %s" d="M%.2f %.2f A76 76 0 %d 1 %.2f %.2f"/>`, class, x0, y0, large, x1, y1)
	}
	var b strings.Builder
	b.WriteString(arc("cold", 40, 66))
	b.WriteString(arc("comfort", 66, 76))
	b.WriteString(arc("hot", 76, 100))
	for f := 40.0; f <= 100; f += 2 {
		major := int(f)%10 == 0
		r0 := 70.0
		if major {
			r0 = 64
		} else if int(f)%10 != 5 {
			r0 = 73
		}
		x0, y0 := pt(f, r0)
		x1, y1 := pt(f, 80)
		cls := "tick"
		if major {
			cls = "tick major"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`, cls, x0, y0, x1, y1)
		if major {
			lx, ly := pt(f, 52)
			fmt.Fprintf(&b, `<text class="glabel" x="%.2f" y="%.2f">%.0f</text>`, lx, ly+4, f)
		}
	}
	return template.HTML(b.String())
}()

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + humanDur(-d)
	case d < time.Minute:
		return "just now"
	default:
		return humanDur(d) + " ago"
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func describeBucket(b time.Duration) string {
	switch {
	case b <= time.Minute:
		return "raw readings"
	case b < time.Hour:
		return fmt.Sprintf("%d-minute averages", int(b.Minutes()))
	default:
		h := b.Hours()
		if h == math.Trunc(h) {
			return fmt.Sprintf("%.0f-hour averages", h)
		}
		return fmt.Sprintf("%.1f-hour averages", h)
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			log.Printf("%s %s (%s)", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
		}
	})
}
