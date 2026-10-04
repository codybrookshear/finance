// Draws the net worth chart. The data comes from data-* attributes (never
// inline script), as comma-separated days (YYYY-MM-DD) and decimal strings.
// Converting to Number here is for plotting only; the server never uses floats.
"use strict";

(function () {
  function drawNetWorth() {
    const el = document.getElementById("networth-chart");
    if (!el || el.dataset.drawn || typeof uPlot === "undefined") return;
    const days = el.dataset.days.split(",").map((d) => Date.parse(d + "T00:00:00Z") / 1000);
    const values = el.dataset.values.split(",").map(Number);
    if (days.length < 2 || days.length !== values.length) return;
    el.dataset.drawn = "1";

    const style = getComputedStyle(el);
    const muted = getComputedStyle(document.documentElement).getPropertyValue("--muted").trim();
    const line = getComputedStyle(document.documentElement).getPropertyValue("--line").trim();
    const money = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", maximumFractionDigits: 0 });
    const axis = { stroke: muted, grid: { stroke: line }, ticks: { stroke: line } };
    const height = () => Math.max(240, Math.min(360, Math.round(el.clientWidth * 0.5)));

    const plot = new uPlot({
      width: el.clientWidth,
      height: height(),
      // Days are calendar dates; label them in UTC so they don't shift.
      tzDate: (ts) => uPlot.tzDate(new Date(ts * 1000), "Etc/UTC"),
      legend: { show: false },
      cursor: { drag: { x: false, y: false } },
      scales: { x: { time: true } },
      axes: [
        axis,
        Object.assign({}, axis, { size: 72, values: (u, splits) => splits.map((v) => money.format(v)) }),
      ],
      series: [{}, { label: "Net worth", stroke: style.color, width: 2, value: (u, v) => (v == null ? "" : money.format(v)) }],
    }, [days, values], el);

    new ResizeObserver(() => plot.setSize({ width: el.clientWidth, height: height() })).observe(el);
  }

  document.addEventListener("DOMContentLoaded", drawNetWorth);
})();
