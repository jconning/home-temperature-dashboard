package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"
)

// Server-side SVG line charts. The SVG is complete without JavaScript; a
// small script (static/app.js) adds the hover crosshair using the JSON
// embedded in data-chart.

type ChartPoint struct {
	T      time.Time
	V      float64
	Lo, Hi float64 // min/max band, used when ChartOpts.Band is set
}

type ChartSeries struct {
	Name   string
	Slot   int
	Points []ChartPoint
}

type ChartOpts struct {
	ID       string
	Label    string // accessible name
	Unit     string // "°F", "%"
	From, To time.Time
	Gap      time.Duration // break the line across gaps longer than this
	Loc      *time.Location
	Band     bool
	MinSpan  float64 // minimum y-axis span
	Decimals int
}

const (
	cw, ch                     = 960.0, 300.0
	padL, padR, padT, padB     = 46.0, 185.0, 14.0, 30.0
	plotX0, plotX1             = padL, cw - padR
	plotY0, plotY1             = padT, ch - padB
	labelMinGap                = 15.0
)

type chartJSON struct {
	X0   float64      `json:"x0"`
	X1   float64      `json:"x1"`
	T0   int64        `json:"t0"`
	T1   int64        `json:"t1"`
	Unit string       `json:"unit"`
	Dec  int          `json:"dec"`
	Gap  int64        `json:"gap"`
	V0   float64      `json:"v0"` // value at the bottom of the plot
	V1   float64      `json:"v1"` // value at the top
	PY0  float64      `json:"py0"`
	PY1  float64      `json:"py1"`
	W    float64      `json:"w"`
	S    []seriesJSON `json:"s"`
}

type seriesJSON struct {
	N    string       `json:"n"`
	Slot int          `json:"slot"`
	P    [][2]float64 `json:"p"`
}

func RenderChart(series []ChartSeries, o ChartOpts) template.HTML {
	hasData := false
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range series {
		for _, p := range s.Points {
			hasData = true
			l, h := p.V, p.V
			if o.Band {
				l, h = p.Lo, p.Hi
			}
			lo, hi = math.Min(lo, l), math.Max(hi, h)
		}
	}
	if !hasData {
		return template.HTML(`<div class="chart-empty"><svg viewBox="0 0 64 64" aria-hidden="true" class="empty-fan"><use href="#fan"/></svg><p>No readings in this window yet. The poller logs a reading every cycle. Check back soon or import a Govee CSV export.</p></div>`)
	}

	// Y scale with "nice" ticks.
	if hi-lo < o.MinSpan {
		mid := (hi + lo) / 2
		lo, hi = mid-o.MinSpan/2, mid+o.MinSpan/2
	}
	step := niceStep((hi - lo) / 5)
	yMin := math.Floor(lo/step) * step
	yMax := math.Ceil(hi/step) * step
	if yMax == yMin {
		yMax = yMin + step
	}
	t0, t1 := o.From.Unix(), o.To.Unix()
	xOf := func(t time.Time) float64 {
		return plotX0 + (plotX1-plotX0)*float64(t.Unix()-t0)/float64(t1-t0)
	}
	yOf := func(v float64) float64 {
		return plotY1 - (plotY1-plotY0)*(v-yMin)/(yMax-yMin)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="%s">`,
		cw, ch, template.HTMLEscapeString(o.Label))

	// Grid + y axis labels.
	b.WriteString(`<g class="grid">`)
	for v := yMin; v <= yMax+step/2; v += step {
		y := yOf(v)
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, plotX0, plotX1, y, y)
		fmt.Fprintf(&b, `<text class="ytick" x="%.1f" y="%.1f">%s</text>`, plotX0-8, y+4, trimFloat(v, step))
	}
	b.WriteString(`</g>`)

	// X axis ticks.
	fmt.Fprintf(&b, `<line class="baseline" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, plotX0, plotX1, plotY1, plotY1)
	b.WriteString(`<g class="xticks">`)
	for _, tk := range timeTicks(o.From, o.To, o.Loc) {
		x := xOf(tk.t)
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, x, x, plotY1, plotY1+5)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f">%s</text>`, x, plotY1+19, template.HTMLEscapeString(tk.label))
	}
	b.WriteString(`</g>`)

	cj := chartJSON{X0: plotX0, X1: plotX1, T0: t0, T1: t1, Unit: o.Unit, Dec: o.Decimals, Gap: int64(o.Gap / time.Second),
		V0: yMin, V1: yMax, PY0: plotY0, PY1: plotY1, W: cw}

	type endLabel struct {
		y    float64
		slot int
		text string
		x    float64
		yDot float64
	}
	var labels []endLabel

	b.WriteString(`<g class="series">`)
	for _, s := range series {
		if len(s.Points) == 0 {
			continue
		}
		sj := seriesJSON{N: s.Name, Slot: s.Slot}
		segs := splitOnGaps(s.Points, o.Gap)
		if o.Band {
			for _, seg := range segs {
				if len(seg) < 2 {
					continue
				}
				var pb strings.Builder
				for i, p := range seg {
					cmd := "L"
					if i == 0 {
						cmd = "M"
					}
					fmt.Fprintf(&pb, "%s%.1f %.1f", cmd, xOf(p.T), yOf(p.Hi))
				}
				for i := len(seg) - 1; i >= 0; i-- {
					fmt.Fprintf(&pb, "L%.1f %.1f", xOf(seg[i].T), yOf(seg[i].Lo))
				}
				pb.WriteString("Z")
				fmt.Fprintf(&b, `<path class="band s%d" d="%s"/>`, s.Slot, pb.String())
			}
		}
		for _, seg := range segs {
			var pb strings.Builder
			for i, p := range seg {
				cmd := "L"
				if i == 0 {
					cmd = "M"
				}
				fmt.Fprintf(&pb, "%s%.1f %.1f", cmd, xOf(p.T), yOf(p.V))
			}
			if len(seg) == 1 {
				p := seg[0]
				fmt.Fprintf(&b, `<circle class="dot s%d" cx="%.1f" cy="%.1f" r="3"/>`, s.Slot, xOf(p.T), yOf(p.V))
				continue
			}
			fmt.Fprintf(&b, `<path class="line s%d" d="%s" pathLength="1"/>`, s.Slot, pb.String())
		}
		for _, p := range s.Points {
			sj.P = append(sj.P, [2]float64{float64(p.T.Unix()), round(p.V, o.Decimals)})
		}
		cj.S = append(cj.S, sj)
		last := s.Points[len(s.Points)-1]
		labels = append(labels, endLabel{
			y: yOf(last.V), yDot: yOf(last.V), x: xOf(last.T), slot: s.Slot,
			text: fmt.Sprintf("%s %.*f%s", s.Name, o.Decimals, last.V, o.Unit),
		})
	}
	b.WriteString(`</g>`)

	// Direct labels at line ends, nudged apart so they don't collide.
	sort.Slice(labels, func(i, j int) bool { return labels[i].y < labels[j].y })
	for i := 1; i < len(labels); i++ {
		if labels[i].y-labels[i-1].y < labelMinGap {
			labels[i].y = labels[i-1].y + labelMinGap
		}
	}
	if n := len(labels); n > 0 && labels[n-1].y > plotY1 {
		shift := labels[n-1].y - plotY1
		for i := range labels {
			labels[i].y -= shift
		}
	}
	b.WriteString(`<g class="endlabels">`)
	for _, l := range labels {
		fmt.Fprintf(&b, `<circle class="keydot s%d" cx="%.1f" cy="%.1f" r="4"/>`, l.slot, plotX1+14, l.y)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f">%s</text>`, plotX1+24, l.y+4, template.HTMLEscapeString(l.text))
	}
	b.WriteString(`</g>`)

	// Hover layer (driven by app.js).
	fmt.Fprintf(&b, `<g class="hover" hidden><line class="xhair" y1="%.1f" y2="%.1f"/></g>`, plotY0, plotY1)
	fmt.Fprintf(&b, `<rect class="hit" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, plotX0, plotY0, plotX1-plotX0, plotY1-plotY0)
	b.WriteString(`</svg>`)

	js, _ := json.Marshal(cj)
	return template.HTML(fmt.Sprintf(`<div class="chart-wrap" id="%s" data-chart="%s">%s<div class="tip" hidden></div></div>`,
		template.HTMLEscapeString(o.ID), template.HTMLEscapeString(string(js)), b.String()))
}

func splitOnGaps(pts []ChartPoint, gap time.Duration) [][]ChartPoint {
	var segs [][]ChartPoint
	start := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].T.Sub(pts[i-1].T) > gap {
			segs = append(segs, pts[start:i])
			start = i
		}
	}
	return append(segs, pts[start:])
}

func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

func trimFloat(v, step float64) string {
	if step >= 1 {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func round(v float64, dec int) float64 {
	p := math.Pow(10, float64(dec))
	return math.Round(v*p) / p
}

type tick struct {
	t     time.Time
	label string
}

// timeTicks picks calendar-aligned ticks (in loc) giving at most ~8 labels.
func timeTicks(from, to time.Time, loc *time.Location) []tick {
	span := to.Sub(from)
	hourSteps := []int{1, 2, 3, 6, 12}
	daySteps := []int{1, 2, 7, 14}
	var out []tick
	f := from.In(loc)
	midnight := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)

	for _, h := range hourSteps {
		if span/(time.Duration(h)*time.Hour) <= 8 {
			for t := midnight; !t.After(to); t = t.Add(time.Duration(h) * time.Hour) {
				lt := t.In(loc)
				if t.Before(from) || lt.Hour()%h != 0 {
					continue
				}
				label := lt.Format("3 PM")
				if lt.Hour() == 0 {
					label = lt.Format("Mon 2")
				}
				out = append(out, tick{t, label})
			}
			return out
		}
	}
	for _, d := range daySteps {
		if span/(time.Duration(d)*24*time.Hour) <= 8 {
			for t := midnight; !t.After(to); t = t.AddDate(0, 0, d) {
				if t.Before(from) {
					continue
				}
				out = append(out, tick{t, t.Format("Jan 2")})
			}
			return out
		}
	}
	// Months.
	months := 1
	for span/(time.Duration(months)*30*24*time.Hour) > 8 {
		months++
	}
	first := time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, loc)
	for t := first; !t.After(to); t = t.AddDate(0, months, 0) {
		if t.Before(from) {
			continue
		}
		label := t.Format("Jan")
		if t.Month() == time.January {
			label = t.Format("Jan 2006")
		}
		out = append(out, tick{t, label})
	}
	return out
}
