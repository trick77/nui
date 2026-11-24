// The charts, hand-drawn as in the sibling repos: thin marks, 2px lines,
// one axis, recessive grid, a tooltip on every mark, fixed series colours.
// Scaling only: every value drawn is the backend's.

import { type ReactNode, useId, useRef, useState } from "react";
import type { Axis, Change, ClassKey, Tick, Verdict } from "./api";
import { SEP, changeText, num, pct, value as fmtValue } from "./format";
import { useWidth } from "./hooks";
import { CLASSES, Delta, Tip, useTip } from "./ui";

const STEPS = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10];

// The axis' lines from 0 to at or above v, at most five of them a round step
// apart: whole numbers for a count, tenths for a share and never past 100.
export function scaleLines(v: number, kind: "sum" | "rate"): number[] {
  if (!(v > 0)) return [0, 1];
  const p = Math.pow(10, Math.floor(Math.log10(v / 4)));
  let step =
    STEPS.map((s) => Number((s * p).toPrecision(2))).find(
      // A share is written to a tenth.
      (s) => s * 4 >= v && Number.isInteger(kind === "rate" ? s * 10 : s),
    ) ?? (kind === "rate" ? 0.1 : 1);
  if (kind === "rate" && v <= 100 && step * Math.ceil(v / step) > 100)
    step = 25;
  const n = Math.max(1, Math.ceil(v / step - 1e-9));
  return Array.from({ length: n + 1 }, (_, i) =>
    Number((i * step).toPrecision(12)),
  );
}

// A mono character of the axis' texts, in px.
const CHAR = 6.4;

// The labels that fit, every so-manyth from the last one back, so they never
// overprint; x is a place's. One near the right edge moves in.
function TickLabels({
  ticks,
  x,
  right,
  y,
}: {
  ticks: Tick[];
  x: (place: number) => number;
  right: number;
  y: number;
}) {
  const named = ticks.filter((t) => t.label);
  if (!named.length) return null;
  const last = named.length - 1;
  const longest = Math.max(...named.map((t) => t.label.length)) * CHAR;
  const apart = last ? (x(named[last].at) - x(named[0].at)) / last : Infinity;
  const step = Math.max(1, Math.ceil((longest + 16) / Math.max(1, apart)));
  return named.map((t, i) =>
    (last - i) % step ? null : (
      <text
        key={t.at}
        x={Math.min(x(t.at), right - (t.label.length * CHAR) / 2)}
        y={y}
        textAnchor="middle"
      >
        {t.label}
      </text>
    ),
  );
}

// The rules where a day, a week or a month begins.
function TickRules({
  ticks,
  x,
  top,
  bottom,
}: {
  ticks: Tick[];
  x: (place: number) => number;
  top: number;
  bottom: number;
}) {
  return ticks.map((t) =>
    t.line && t.at > 0 ? (
      <line
        key={t.at}
        x1={x(t.at)}
        x2={x(t.at)}
        y1={top}
        y2={bottom}
        stroke="var(--color-grid-minor)"
        strokeDasharray="2 4"
      />
    ) : null,
  );
}

type Figures = (number | null)[];

// The comparison series, or null when there is none: with the comparison
// off the backend sends an empty one.
const series = (s: Figures | null | undefined) => (s?.length ? s : null);

// The area under a series, one for each of its unbroken runs.
function areaPath(
  a: Figures,
  x: (i: number) => number,
  y: (v: number) => number,
  base: number,
): string {
  let d = "";
  for (let i = 0; i < a.length; i++) {
    if (a[i] == null) continue;
    const from = i;
    while (i + 1 < a.length && a[i + 1] != null) i++;
    if (i > from)
      d += `${brokenPath(a, from, i + 1, x, y)}L${x(i)} ${base}L${x(from)} ${base}Z`;
  }
  return d;
}

// What a series has: it may lack a slot's figure (null).
const known = (a: Figures) => a.filter((v) => v != null);

// A series as a path over its points, broken where a figure is missing; one
// that stands alone is a dot (its line needs a round cap).
function brokenPath(
  a: Figures,
  from: number,
  to: number,
  x: (i: number) => number,
  y: (v: number) => number,
): string {
  let d = "";
  for (let i = from; i < to; i++) {
    const v = a[i];
    if (v == null) continue;
    const start = i === from || a[i - 1] == null;
    const alone = start && (i === to - 1 || a[i + 1] == null);
    d += `${start ? "M" : "L"}${x(i).toFixed(1)} ${y(v).toFixed(1)}${alone ? "h0" : ""}`;
  }
  return d;
}

// A tile's small trend: this period's mean in s1, the previous one's grey.
export function Spark({
  cur,
  prev: prevIn,
}: {
  cur: Figures;
  prev: Figures | null;
}) {
  const prev = series(prevIn);
  const W = 84;
  const H = 30;
  const n = cur.length;
  const all = known([...cur, ...(prev ?? [])]);
  if (n < 2 || !all.length)
    return <svg width={W} height={H} aria-hidden="true" />;
  const max = Math.max(...all);
  const min = Math.min(...all) * 0.9;
  const span = max - min || 1;
  const pts = (a: Figures) =>
    brokenPath(
      a,
      0,
      a.length,
      (i) => (i * W) / (n - 1),
      (v) => H - 2 - ((H - 4) * (v - min)) / span,
    );
  return (
    <svg
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      aria-hidden="true"
      className="flex-none"
    >
      {prev && (
        <path
          d={pts(prev)}
          fill="none"
          stroke="var(--color-reference)"
          strokeWidth="1.25"
          strokeLinecap="round"
        />
      )}
      <path
        d={pts(cur)}
        fill="none"
        stroke="var(--color-s1)"
        // More than a point a pixel: a thick line of a share is a block.
        strokeWidth={n > W * 2 ? 1.1 : 1.75}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

// A bar from the base up: rounded on top, no taller than it is wide there.
function barPath(x0: number, w: number, base: number, top: number): string {
  const h = base - top;
  const r = Math.min(3, w / 2, h);
  const f = (v: number) => v.toFixed(1);
  return `M${f(x0)} ${f(base)}v${f(r - h)}q0 ${f(-r)} ${f(r)} ${f(-r)}h${f(w - 2 * r)}q${f(r)} 0 ${f(r)} ${f(r)}v${f(h - r)}z`;
}

// A figure that counts: not a share, and not a measure (it has a unit).
export const isCount = (kind: "sum" | "rate", unit = "") =>
  kind === "sum" && !unit;

// The overview's main chart, two layers of one figure: every slot pale (the
// detail), and over it the moving mean (the trend), which the previous
// period's grey mean is compared with. A count's slots are bars: a line
// through counts of 0 to 4 is a saw that crosses the mean all the time. A
// share's or a percentile's are a line with an area under it. The weekends
// are tinted, the time before the domain's first figure is not a quiet one,
// the open slot is paler, over a hatch.
export function MainChart({
  labels,
  cur,
  mean,
  prev: prevIn,
  delta = [],
  meanLabel,
  axis,
  first = null,
  kind,
  unit = "",
  open,
  title,
  prevLabel,
}: {
  labels: string[];
  cur: Figures;
  mean: Figures;
  prev: Figures | null;
  delta?: Figures;
  meanLabel: string;
  axis: Axis;
  first?: number | null;
  kind: "sum" | "rate";
  // Shown after every figure; a rate has its own.
  unit?: string;
  open: boolean;
  title: string;
  prevLabel: string;
}) {
  const prev = series(prevIn);
  const ref = useRef<HTMLDivElement>(null);
  const W = Math.max(320, useWidth(ref, 900));
  const id = useId().replace(/:/g, "");
  const [hover, setHover] = useState<number | null>(null);
  const { tip, show, hide } = useTip();
  const n = cur.length;
  // The previous period's line stands without this one's.
  if (![cur, mean, prev ?? []].some((s) => known(s).length))
    return (
      <div ref={ref} className="py-10 text-center text-[13px] text-muted">
        No data in this range yet.
      </div>
    );
  const H = 320;
  const T = 26;
  const B = 26;
  const R = 14;
  const tail = kind === "sum" && unit ? ` ${unit}` : "";
  const fmt = (v: number | null | undefined) =>
    fmtValue(v, kind) + (v != null ? tail : "");
  // A mean of small counts is a fraction.
  const fmtMean = (v: number | null | undefined) =>
    v == null || kind === "rate" || v >= 10 || Number.isInteger(v)
      ? fmt(v)
      : num(v, v < 1 ? 2 : 1) + tail;
  const lines = scaleLines(
    Math.max(0, ...known([...cur, ...mean, ...(prev ?? [])])),
    kind,
  );
  const hi = lines[lines.length - 1];
  const L = Math.max(52, fmt(hi).length * CHAR + 14);
  const base = H - B;
  // A place on the axis, and the point of a slot: its middle.
  const at = (place: number) => L + ((W - L - R) * place) / n;
  const x = (i: number) => at(i + 0.5);
  const y = (v: number) => T + (base - T) * (1 - v / hi);
  const path = (a: Figures, from = 0, to = a.length) =>
    brokenPath(a, from, to, x, y);
  const solidTo = open ? n - 1 : n;
  const bars = isCount(kind, unit);
  const slotW = (W - L - R) / n;
  // Never past its slot: a slot of nothing would go under its neighbours.
  const barW = Math.min(
    slotW,
    Math.max(1.5, Math.min(22, slotW - (slotW > 6 ? 2 : 1))),
  );
  const bar = (i: number) =>
    cur[i] ? barPath(x(i) - barW / 2, barW, base, y(cur[i] ?? 0)) : "";
  // The largest, named: the first of equals, of a figure that is not a share
  // (a slot of one request reaches 100 %).
  const peak = kind === "sum" ? cur.indexOf(Math.max(...known(cur))) : -1;
  const peakLabel =
    peak >= 0 && (cur[peak] ?? 0) > 0
      ? `${fmt(cur[peak])}${SEP}${labels[peak]}`
      : "";
  const half = (peakLabel.length * CHAR) / 2;
  const empty = first == null ? 0 : at(first) - L;

  const onMove = (e: React.PointerEvent<SVGRectElement>) => {
    const box = (
      e.currentTarget.ownerSVGElement ?? e.currentTarget
    ).getBoundingClientRect();
    const rel = ((e.clientX - box.left - L) / (W - L - R)) * n;
    const i = Math.max(0, Math.min(n - 1, Math.floor(rel)));
    setHover(i);
    const rows: [string, string][] = [
      [title, fmt(cur[i])],
      [`${meanLabel} average`, fmtMean(mean[i])],
    ];
    if (prev) {
      const d = delta[i];
      const by =
        d == null ? "" : changeText(kind === "rate" ? { pp: d } : { pct: d });
      rows.push([
        `${prevLabel} average`,
        fmtMean(prev[i]) + (by ? `${SEP}${by}` : ""),
      ]);
    }
    show(
      e,
      labels[i] + (open && i === n - 1 ? `${SEP}still filling` : ""),
      rows,
    );
  };

  const last = cur[n - 1];
  const hovered = hover == null || hover >= n ? null : hover;
  const dot = (i: number, v: number | null | undefined, r = 4) =>
    v != null && (
      <circle
        cx={x(i)}
        cy={y(v)}
        r={r}
        fill="var(--color-s1)"
        stroke="var(--color-panel)"
        strokeWidth="2"
      />
    );
  return (
    <div ref={ref} className="relative">
      <svg
        width={W}
        height={H}
        viewBox={`0 0 ${W} ${H}`}
        role="img"
        aria-label={`${title}, ${labels[0]} to ${labels[n - 1]}`}
        className="block max-w-full"
      >
        <defs>
          <linearGradient id={`ag${id}`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" stopColor="var(--color-s1)" stopOpacity=".18" />
            <stop offset="1" stopColor="var(--color-s1)" stopOpacity="0" />
          </linearGradient>
          <pattern
            id={`hatch${id}`}
            width="7"
            height="7"
            patternUnits="userSpaceOnUse"
            patternTransform="rotate(45)"
          >
            <line
              x1="0"
              y1="0"
              x2="0"
              y2="7"
              stroke="var(--color-grid)"
              strokeWidth="2"
            />
          </pattern>
        </defs>
        {axis.weekends.map(([a, b]) => (
          <rect
            key={a}
            data-testid="weekend"
            x={at(a)}
            y={T}
            width={at(b) - at(a)}
            height={base - T}
            fill="var(--color-ink)"
            opacity=".028"
          />
        ))}
        {empty > 0 && (
          <g data-testid="no-data">
            <rect
              x={L}
              y={T}
              width={empty}
              height={base - T}
              fill="var(--color-bg)"
              opacity=".6"
            />
            {empty > 100 && (
              <text
                x={L + empty / 2}
                // Between two lines, never through one.
                y={y((lines[0] + lines[1]) / 2) + 3.5}
                textAnchor="middle"
              >
                no data yet
              </text>
            )}
          </g>
        )}
        <TickRules ticks={axis.ticks} x={at} top={T} bottom={base} />
        {lines.map((v) => (
          <g key={v}>
            <line
              x1={L}
              x2={W - R}
              y1={y(v)}
              y2={y(v)}
              stroke={v ? "var(--color-grid-minor)" : "var(--color-axis)"}
            />
            <text x={L - 8} y={y(v) + 3.5} textAnchor="end">
              {fmt(v)}
            </text>
          </g>
        ))}
        <TickLabels ticks={axis.ticks} x={at} right={W} y={H - 7} />
        {open && (
          <rect
            className="chart-fill"
            x={at(n - 1)}
            y={T}
            width={at(n) - at(n - 1)}
            height={base - T}
            fill={`url(#hatch${id})`}
            opacity=".55"
          />
        )}
        {/* Keyed: from one to the other, a layer fades in whole. */}
        {bars ? (
          <g key="bars">
            <path
              className="chart-fill"
              data-testid="slots"
              d={cur
                .slice(0, solidTo)
                .map((_, i) => bar(i))
                .join("")}
              fill="var(--color-s1)"
              fillOpacity=".42"
            />
            {open && (
              <path
                className="chart-fill"
                data-testid="open-slot"
                d={bar(n - 1)}
                fill="var(--color-s1)"
                fillOpacity=".22"
              />
            )}
          </g>
        ) : (
          <g key="line">
            <path
              className="chart-fill"
              d={areaPath(cur.slice(0, solidTo), x, y, base)}
              fill={`url(#ag${id})`}
            />
            <path
              className="chart-fill"
              data-testid="slots"
              d={path(cur, 0, solidTo)}
              fill="none"
              stroke="var(--color-s1)"
              strokeOpacity=".6"
              strokeWidth="1.25"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            {open && n >= 2 && (
              <path
                className="chart-fill"
                d={path(cur, n - 2, n)}
                fill="none"
                stroke="var(--color-s1)"
                strokeOpacity=".7"
                strokeWidth="1.25"
                strokeDasharray="2 3"
              />
            )}
          </g>
        )}
        {prev && (
          <path
            className="chart-line"
            data-testid="prev"
            pathLength={1}
            d={path(prev)}
            fill="none"
            stroke="var(--color-reference)"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        )}
        <path
          className="chart-line"
          data-testid="mean"
          pathLength={1}
          d={path(mean)}
          fill="none"
          stroke="var(--color-s1)"
          strokeWidth="2.25"
          strokeLinecap="round"
          strokeLinejoin="round"
          style={{ filter: "drop-shadow(0 0 6px rgba(57,135,229,.55))" }}
        />
        {peakLabel && (
          <g className="chart-fill" data-testid="peak">
            {!bars && dot(peak, cur[peak], 3.5)}
            <text
              x={Math.max(L + half, Math.min(W - half, x(peak)))}
              y={y(cur[peak] ?? 0) - 9}
              textAnchor="middle"
              style={{ fill: "var(--color-ink-dim)" }}
            >
              {peakLabel}
            </text>
          </g>
        )}
        {/* Now: the right edge's one point. */}
        {!bars && last != null && (open || peak !== n - 1) && (
          <circle
            cx={x(n - 1)}
            cy={y(last)}
            r="3.5"
            fill={open ? "var(--color-panel)" : "var(--color-s1)"}
            stroke={open ? "var(--color-s1)" : "var(--color-panel)"}
            strokeWidth="1.5"
          />
        )}
        {hovered != null && (
          <g pointerEvents="none">
            <line
              x1={x(hovered)}
              x2={x(hovered)}
              y1={T}
              y2={base}
              stroke="var(--color-axis)"
            />
            {bars ? (
              <path
                data-testid="hovered"
                d={bar(hovered)}
                fill="var(--color-s1)"
                fillOpacity=".5"
              />
            ) : (
              dot(hovered, cur[hovered], 3.5)
            )}
            {dot(hovered, mean[hovered])}
          </g>
        )}
        {/* Pointer events: a finger reads the chart as a mouse does. */}
        <rect
          data-testid="chart-hit"
          x={L}
          y={T}
          width={W - L - R}
          height={base - T}
          fill="transparent"
          style={{ touchAction: "pan-y" }}
          onPointerDown={onMove}
          onPointerMove={onMove}
          onPointerLeave={() => {
            setHover(null);
            hide();
          }}
        />
      </svg>
      <Tip tip={tip} />
    </div>
  );
}

// Requests per class per bar, stacked in the fixed class order.
export function StackChart({
  labels,
  series,
  classes = CLASSES,
  axis,
  open,
  partial,
}: {
  labels: string[];
  series: Record<ClassKey, number[]>;
  // Those the tooltip lists.
  classes?: typeof CLASSES;
  axis: Axis;
  open: boolean;
  partial: boolean[];
}) {
  const ref = useRef<HTMLDivElement>(null);
  const W = Math.max(280, useWidth(ref, 560));
  const { tip, show, hide } = useTip();
  const n = labels.length;
  const H = 250;
  const R = 8;
  const T = 12;
  const B = 26;
  if (!n)
    return (
      <div ref={ref} className="py-10 text-center text-[13px] text-muted">
        No requests in this range yet.
      </div>
    );
  const lines = scaleLines(
    Math.max(
      ...labels.map((_, i) =>
        CLASSES.reduce((a, c) => a + (series[c.key]?.[i] ?? 0), 0),
      ),
    ),
    "sum",
  );
  const hi = lines[lines.length - 1];
  const L = Math.max(52, num(hi).length * CHAR + 14);
  const slot = (W - L - R) / n;
  const bw = Math.max(2, Math.min(34, slot - (slot > 8 ? 4 : 1)));
  const h = (v: number) => ((H - T - B) * v) / hi;
  return (
    <div ref={ref}>
      <svg
        width={W}
        height={H}
        viewBox={`0 0 ${W} ${H}`}
        role="img"
        aria-label="Requests by class"
        className="block max-w-full"
      >
        {lines.map((v) => (
          <g key={v}>
            <line
              x1={L}
              x2={W - R}
              y1={H - B - h(v)}
              y2={H - B - h(v)}
              stroke={v ? "var(--color-grid-minor)" : "var(--color-axis)"}
            />
            <text x={L - 8} y={H - B - h(v) + 3.5} textAnchor="end">
              {num(v)}
            </text>
          </g>
        ))}
        <TickLabels
          ticks={axis.ticks}
          x={(place) => L + slot * place}
          right={W}
          y={H - 7}
        />
        {labels.map((lab, i) => {
          let base = H - B;
          const gap = bw > 5 ? 2 : 0;
          const x0 = L + slot * i + (slot - bw) / 2;
          const present = CLASSES.filter(
            (c) => h(series[c.key]?.[i] ?? 0) >= 1,
          );
          const top = present[present.length - 1]?.key;
          const note = !partial[i]
            ? ""
            : `${SEP}${open && i === n - 1 ? "still filling" : "part of a week"}`;
          return (
            <g key={i}>
              {present.map((c) => {
                const hh0 = h(series[c.key][i]);
                const hh = Math.max(1, hh0 - gap);
                const rr = c.key === top ? Math.min(4, bw / 2, hh) : 0;
                const d = `M${x0} ${base}v${-(hh - rr)}q0 ${-rr} ${rr} ${-rr}h${bw - 2 * rr}q${rr} 0 ${rr} ${rr}v${hh - rr}z`;
                base -= hh0;
                return (
                  <path
                    key={c.key}
                    d={d}
                    className="chart-bar"
                    style={{ animationDelay: `${Math.round((i * 380) / n)}ms` }}
                    fill={c.color}
                    opacity={partial[i] ? 0.55 : 1}
                  />
                );
              })}
              <rect
                data-testid="stack-hit"
                x={L + slot * i}
                y={T}
                width={slot}
                height={H - T - B}
                fill="transparent"
                onMouseMove={(e) =>
                  show(
                    e,
                    lab + note,
                    classes.map((c) => [c.label, num(series[c.key]?.[i] ?? 0)]),
                  )
                }
                onMouseLeave={hide}
              />
            </g>
          );
        })}
      </svg>
      <Tip tip={tip} />
    </div>
  );
}

export type BarRow = {
  key: string;
  label: ReactNode;
  value: number;
  share: number | null;
  change: Change;
  verdict: Verdict | null;
  onClick?: () => void;
  title?: string;
};

const SHADE = [
  "var(--color-seq-4)",
  "var(--color-seq-3)",
  "var(--color-seq-2)",
  "var(--color-seq-1)",
];

// Ranked rows, a bar behind each label scaled to the largest, the shade
// stepping down the sequential blue ramp by rank; "Other" grey.
export function BarRows({
  rows,
  other,
  otherLabel,
}: {
  rows: BarRow[];
  other?: { count: number; value: number } | null;
  otherLabel?: string;
}) {
  const max = Math.max(1, ...rows.map((r) => r.value), other?.value ?? 0);
  const all = [...rows];
  return (
    <div className="bl">
      {all.map((r, i) => (
        <div
          key={i}
          className={`r ${r.onClick ? "click" : ""}`}
          role={r.onClick ? "button" : undefined}
          tabIndex={r.onClick ? 0 : undefined}
          title={r.title}
          onClick={r.onClick}
          onKeyDown={
            r.onClick
              ? (e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    r.onClick!();
                  }
                }
              : undefined
          }
        >
          <i
            style={{
              width: `${(r.value / max) * 100}%`,
              background: SHADE[Math.min(i, 3)],
              opacity: 0.62,
              animationDelay: `${i * 50}ms`,
            }}
          />
          <span className="truncate">{r.label}</span>
          <span className="val">
            {num(r.value)}
            {r.share != null && <small>{pct(r.share)}</small>}
            <b>
              <Delta change={r.change} verdict={r.verdict} />
            </b>
          </span>
        </div>
      ))}
      {other && other.count > 0 && (
        <div className="r">
          <i
            style={{
              width: `${(other.value / max) * 100}%`,
              background: "var(--color-other)",
              opacity: 0.45,
              animationDelay: `${all.length * 50}ms`,
            }}
          />
          <span className="text-muted">
            {otherLabel ?? `Other${SEP}${num(other.count)}`}
          </span>
          <span className="val">
            {num(other.value)}
            <b />
          </span>
        </div>
      )}
    </div>
  );
}

export type StripPart = {
  key: string;
  label: string;
  share: number;
  color: string;
  value?: number;
};

// A whole split into its parts, with a legend of the shares.
export function Strip({ parts, label }: { parts: StripPart[]; label: string }) {
  const { tip, show, hide } = useTip();
  return (
    <div>
      <div className="strip" role="img" aria-label={label}>
        {parts.map((p, i) => (
          <i
            key={p.key}
            data-testid="strip-part"
            style={{
              flex: Math.max(p.share, 0.0001),
              background: p.color,
              animationDelay: `${i * 70}ms`,
            }}
            onMouseMove={(e) =>
              show(e, p.label, [
                ["Share", pct(p.share)],
                ...(p.value != null
                  ? [["Count", num(p.value)] as [string, string]]
                  : []),
              ])
            }
            onMouseLeave={hide}
          />
        ))}
      </div>
      <div className="legend">
        {parts.map((p) => (
          <span key={p.key}>
            <i className="sw" style={{ background: p.color }} />
            {p.label} <b>{pct(p.share)}</b>
          </span>
        ))}
      </div>
      <Tip tip={tip} />
    </div>
  );
}

const LEVEL = [
  "var(--color-cell-empty)",
  "var(--color-seq-1)",
  "var(--color-seq-2)",
  "var(--color-seq-3)",
  "var(--color-seq-4)",
];

// 24 hours in the backend's levels 0 to 4.
export function Hours({ levels }: { levels: number[] }) {
  return (
    <div>
      <div className="hours" role="img" aria-label="Activity by hour">
        {levels.map((l, h) => (
          <i
            key={h}
            title={`${String(h).padStart(2, "0")}:00`}
            style={{ background: LEVEL[Math.max(0, Math.min(4, l))] }}
          />
        ))}
      </div>
      <div className="mt-1 flex justify-between font-mono text-[10.5px] text-muted">
        <span>00</span>
        <span>06</span>
        <span>12</span>
        <span>18</span>
        <span>23</span>
      </div>
    </div>
  );
}

export { LEVEL };
