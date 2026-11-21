// Hover crosshair + tooltip for the server-rendered SVG charts.
// The chart is fully usable without this; it only adds the readout layer.
(function () {
  "use strict";

  function nearest(points, t) {
    let lo = 0, hi = points.length - 1;
    if (hi < 0) return null;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (points[mid][0] < t) lo = mid; else hi = mid;
    }
    return Math.abs(points[lo][0] - t) <= Math.abs(points[hi][0] - t) ? points[lo] : points[hi];
  }

  function fmtTime(sec) {
    return new Date(sec * 1000).toLocaleString(undefined, {
      weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit",
    });
  }

  function setup(wrap) {
    const cfg = JSON.parse(wrap.dataset.chart);
    const svg = wrap.querySelector("svg.chart");
    const hit = svg.querySelector(".hit");
    const hover = svg.querySelector(".hover");
    const xhair = hover.querySelector(".xhair");
    const tip = wrap.querySelector(".tip");
    const NS = "http://www.w3.org/2000/svg";
    const dots = cfg.s.map(function (s) {
      const c = document.createElementNS(NS, "circle");
      c.setAttribute("r", "4.5");
      c.setAttribute("class", "hdot s" + s.slot);
      hover.appendChild(c);
      return c;
    });

    const xOf = (t) => cfg.x0 + (cfg.x1 - cfg.x0) * (t - cfg.t0) / (cfg.t1 - cfg.t0);
    const yOf = (v) => cfg.py1 - (cfg.py1 - cfg.py0) * (v - cfg.v0) / (cfg.v1 - cfg.v0);

    function move(ev) {
      const pt = svg.createSVGPoint();
      pt.x = ev.clientX; pt.y = ev.clientY;
      const p = pt.matrixTransform(svg.getScreenCTM().inverse());
      const t = cfg.t0 + (p.x - cfg.x0) / (cfg.x1 - cfg.x0) * (cfg.t1 - cfg.t0);

      // Snap to the closest actual sample across all series.
      let snap = null;
      cfg.s.forEach(function (s) {
        const n = nearest(s.p, t);
        if (n && (snap === null || Math.abs(n[0] - t) < Math.abs(snap - t))) snap = n[0];
      });
      if (snap === null || Math.abs(snap - t) > cfg.gap) { hide(); return; }

      let rows = "";
      cfg.s.forEach(function (s, i) {
        const n = nearest(s.p, snap);
        if (!n || Math.abs(n[0] - snap) > cfg.gap / 2) { dots[i].setAttribute("visibility", "hidden"); return; }
        dots[i].setAttribute("visibility", "visible");
        dots[i].setAttribute("cx", xOf(n[0]));
        dots[i].setAttribute("cy", yOf(n[1]));
        rows += '<div class="tr"><span class="swatch s' + s.slot + '"></span>' + escapeHTML(s.n) +
          '<span class="tv">' + n[1].toFixed(cfg.dec) + cfg.unit + "</span></div>";
      });
      const x = xOf(snap);
      xhair.setAttribute("x1", x); xhair.setAttribute("x2", x);
      hover.removeAttribute("hidden");

      tip.innerHTML = '<div class="tt">' + fmtTime(snap) + "</div>" + rows;
      tip.hidden = false;
      const svgBox = svg.getBoundingClientRect();
      const wrapBox = wrap.getBoundingClientRect();
      const px = svgBox.left - wrapBox.left + wrap.scrollLeft + x / cfg.w * svgBox.width;
      const tw = tip.offsetWidth;
      let left = px + 14;
      if (left + tw > wrap.scrollLeft + wrap.clientWidth - 4) left = px - tw - 14;
      tip.style.left = Math.max(4, left) + "px";
    }

    function hide() {
      hover.setAttribute("hidden", "");
      tip.hidden = true;
    }

    hit.addEventListener("pointermove", move);
    hit.addEventListener("pointerdown", move);
    hit.addEventListener("pointerleave", hide);
  }

  function escapeHTML(s) {
    return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  document.querySelectorAll(".chart-wrap[data-chart]").forEach(setup);
})();
