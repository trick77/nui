// The charts, hand-drawn as in the sibling repos: thin marks, 2px lines,
// one axis, recessive grid, a tooltip on every mark, fixed series colours.
// Scaling only: every value drawn is the backend's.

import { type ReactNode, useId, useRef, useState } from "react";
import type { Change, ClassKey, Verdict } from "./api";
import { SEP, num, pct, value as fmtValue } from "./format";
import { useWidth } from "./hooks";
import { CLASSES, Delta, Tip, useTip } from "./ui";

// The axis top: 1, 2 or 5 times a power of ten at or above v.
export function nice(v: number): number {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / p;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * p;
}

// Every step-th label from the last one back, so they never overprint.
function labelStep(n: number, width: number): number {
  return Math.max(1, Math.ceil(n / Math.max(1, width / 80)));
}

// A tile's small trend: this period in s1, the previous one grey. The open
// bucket is left out, it would always read as a drop.
// The comparison series, or null when there is none: with the comparison
// off the backend sends an empty one.
const series = (s: number[] | null | undefined) => (s?.length ? s : null);

export function Spark({
  cur,
  prev: prevIn,
  open,
}: {
  cur: number[];
  prev: number[] | null;
  open: boolean;
}) {
  const prev = series(prevIn);
  const W = 84;
  const H = 30;
  const n = cur.length;
  const shown = open ? cur.slice(0, -1) : cur;
  const all = [...shown, ...(prev ?? [])];
  if (n < 2 || !all.length)
    return <svg width={W} height={H} aria-hidden="true" />;
  const max = Math.max(...all);
  const min = Math.min(...all) * 0.9;
  const span = max - min || 1;
  const pts = (a: number[]) =>
    a
      .map(
        (v, i) =>
          `${((i * W) / (n - 1)).toFixed(1)},${(H - 2 - ((H - 4) * (v - min)) / span).toFixed(1)}`,
      )
      .join(" ");
  return (
    <svg
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      aria-hidden="true"
      className="flex-none"
    >
      {prev && (
        <polyline
          points={pts(prev)}
          fill="none"
          stroke="var(--color-reference)"
          strokeWidth="1.25"
        />
      )}
      <polyline
        points={pts(shown)}
        fill="none"
        stroke="var(--color-s1)"
        strokeWidth="2"
        strokeLinejoin="round"
      />
    </svg>
  );
}

// A bucket's name in a tooltip. An hour's label is its end: 24h ends now, and
// its hours begin and end at now's minute.
const bucketName = (label: string, hourly: boolean, filling: boolean) =>
  (hourly ? `hour to ${label}` : label) +
  (filling ? `${SEP}still filling` : "");

// The overview's main line: this period with an area under it, the
// previous period grey, the median dashed with a label chip, the open
// bucket dashed over a hatch.
export function MainChart({
  labels,
  cur,
  prev: prevIn,
  median,
  kind,
  open,
  hourly = false,
  title,
  prevLabel,
}: {
  labels: string[];
  cur: number[];
  prev: number[] | null;
  median: number | null;
  kind: "sum" | "rate";
  open: boolean;
  hourly?: boolean;
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
  if (!n)
    return (
      <div ref={ref} className="py-10 text-center text-[13px] text-muted">
        No data in this range yet.
      </div>
    );
  const H = 250;
  const L = 52;
  const R = 92;
  const T = 12;
  const B = 26;
  const fmt = (v: number) => fmtValue(v, kind);
  const vals = [...cur, ...(prev ?? []), ...(median != null ? [median] : [])];
  const lo =
    kind === "sum"
      ? 0
      : Math.max(0, Math.floor((Math.min(...vals) * 0.8) / 10) * 10);
  const hi = lo + nice((Math.max(...vals) - lo) * 1.05);
  const x = (i: number) =>
    n < 2 ? L + (W - L - R) / 2 : L + ((W - L - R) * i) / (n - 1);
  const y = (v: number) => T + (H - T - B) * (1 - (v - lo) / (hi - lo));
  const path = (a: number[], from: number, to: number) =>
    a
      .slice(from, to)
      .map(
        (v, i) =>
          `${i ? "L" : "M"}${x(i + from).toFixed(1)} ${y(v).toFixed(1)}`,
      )
      .join("");
  const openLast = open && n >= 2;
  const solidTo = openLast ? n - 1 : n;
  const step = labelStep(n, W - L - R);
  const ticks: number[] = [];
  for (let i = n - 1; i >= 0; i -= step) ticks.push(i);

  const onMove = (e: React.MouseEvent<SVGRectElement>) => {
    const box = (
      e.currentTarget.ownerSVGElement ?? e.currentTarget
    ).getBoundingClientRect();
    const rel =
      n < 2 ? 0 : ((e.clientX - box.left - L) / (W - L - R)) * (n - 1);
    const i = Math.max(0, Math.min(n - 1, Math.round(rel)));
    setHover(i);
    const rows: [string, string][] = [[title, fmt(cur[i])]];
    if (prev) rows.push([prevLabel, prev[i] != null ? fmt(prev[i]) : "–"]);
    if (median != null) rows.push(["Median", fmt(median)]);
    show(e, bucketName(labels[i], hourly, openLast && i === n - 1), rows);
  };

  const medLabel = median != null ? `median ${fmt(median)}` : "";
  return (
    <div ref={ref} className="relative">
      <svg
        width={W}
        height={H}
        viewBox={`0 0 ${W} ${H}`}
        role="img"
        aria-label={`${title}, ${hourly ? `${n} hours` : labels[0]} to ${labels[n - 1]}`}
        className="block max-w-full"
      >
        <defs>
          <linearGradient id={`ag${id}`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" stopColor="var(--color-s1)" stopOpacity=".34" />
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
        {[0, 0.5, 1].map((f) => {
          const v = lo + (hi - lo) * f;
          return (
            <g key={f}>
              <line
                x1={L}
                x2={W - R}
                y1={y(v)}
                y2={y(v)}
                stroke={f ? "var(--color-grid-minor)" : "var(--color-axis)"}
              />
              {/* A count's half-way line between whole numbers (0.5 of
                  an empty range's 1, 2.5 of 5) would print rounded, a
                  wrong figure: the line stays, its label goes. */}
              {(kind !== "sum" || Number.isInteger(v)) && (
                <text x={L - 8} y={y(v) + 3.5} textAnchor="end">
                  {fmt(v)}
                </text>
              )}
            </g>
          );
        })}
        {ticks.map((i) => (
          <text key={i} x={x(i)} y={H - 7} textAnchor="middle">
            {labels[i]}
          </text>
        ))}
        {openLast && (
          <rect
            className="chart-fill"
            x={x(n - 2)}
            y={T}
            width={x(n - 1) - x(n - 2)}
            height={H - T - B}
            fill={`url(#hatch${id})`}
            opacity=".55"
          />
        )}
        {n >= 2 && (
          <path
            className="chart-fill"
            d={`${path(cur, 0, n)}L${x(n - 1)} ${H - B}L${x(0)} ${H - B}Z`}
            fill={`url(#ag${id})`}
          />
        )}
        {prev && n >= 2 && (
          <path
            className="chart-line"
            pathLength={1}
            d={path(prev, 0, n)}
            fill="none"
            stroke="var(--color-reference)"
            strokeWidth="1.5"
            strokeLinejoin="round"
          />
        )}
        {median != null && (
          <g>
            <line
              x1={L}
              x2={W - R}
              y1={y(median)}
              y2={y(median)}
              stroke="var(--color-ink-dim)"
              strokeWidth="1.5"
              strokeDasharray="5 4"
            />
            <rect
              x={W - R + 5}
              y={y(median) - 9}
              width={medLabel.length * 6.4 + 12}
              height="18"
              rx="5"
              fill="var(--color-panel)"
              stroke="var(--color-border)"
            />
            <text
              x={W - R + 11}
              y={y(median) + 3.5}
              style={{ fill: "var(--color-ink-dim)" }}
            >
              {medLabel}
            </text>
          </g>
        )}
        {solidTo >= 2 && (
          <path
            className="chart-line"
            pathLength={1}
            d={path(cur, 0, solidTo)}
            fill="none"
            stroke="var(--color-s1)"
            strokeWidth="2.25"
            strokeLinejoin="round"
            style={{ filter: "drop-shadow(0 0 6px rgba(57,135,229,.55))" }}
          />
        )}
        {openLast && (
          <path
            className="chart-fill"
            d={path(cur, n - 2, n)}
            fill="none"
            stroke="var(--color-s1)"
            strokeWidth="2"
            strokeDasharray="3 4"
          />
        )}
        <circle
          cx={x(solidTo - 1)}
          cy={y(cur[solidTo - 1])}
          r="4"
          fill="var(--color-s1)"
          stroke="var(--color-panel)"
          strokeWidth="2"
        />
        {openLast && (
          <circle
            cx={x(n - 1)}
            cy={y(cur[n - 1])}
            r="3.5"
            fill="var(--color-panel)"
            stroke="var(--color-s1)"
            strokeWidth="1.5"
          />
        )}
        {hover != null && (
          <g pointerEvents="none">
            <line
              x1={x(hover)}
              x2={x(hover)}
              y1={T}
              y2={H - B}
              stroke="var(--color-axis)"
            />
            <circle
              cx={x(hover)}
              cy={y(cur[hover])}
              r="4"
              fill="var(--color-s1)"
              stroke="var(--color-panel)"
              strokeWidth="2"
            />
          </g>
        )}
        <rect
          data-testid="chart-hit"
          x={L}
          y={T}
          width={W - L - R}
          height={H - T - B}
          fill="transparent"
          onMouseMove={onMove}
          onMouseLeave={() => {
            setHover(null);
            hide();
          }}
        />
      </svg>
      <Tip tip={tip} />
    </div>
  );
}

// Requests per class per bucket, stacked in the fixed class order.
export function StackChart({
  labels,
  series,
  open,
  hourly = false,
}: {
  labels: string[];
  series: Record<ClassKey, number[]>;
  open: boolean;
  hourly?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const W = Math.max(280, useWidth(ref, 560));
  const { tip, show, hide } = useTip();
  const n = labels.length;
  const H = 250;
  const L = 52;
  const R = 8;
  const T = 12;
  const B = 26;
  if (!n)
    return (
      <div ref={ref} className="py-10 text-center text-[13px] text-muted">
        No requests in this range yet.
      </div>
    );
  const stackTop = labels.map((_, i) =>
    CLASSES.reduce((a, c) => a + (series[c.key]?.[i] ?? 0), 0),
  );
  const hi = nice(Math.max(...stackTop) * 1.02);
  const slot = (W - L - R) / n;
  const bw = Math.max(2, Math.min(34, slot - (slot > 8 ? 4 : 1)));
  const h = (v: number) => ((H - T - B) * v) / hi;
  const step = labelStep(n, W - L - R);
  const ticks: number[] = [];
  for (let i = n - 1; i >= 0; i -= step) ticks.push(i);
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
        {[0, 0.5, 1].map((f) => {
          const yy = H - B - h(hi * f);
          return (
            <g key={f}>
              <line
                x1={L}
                x2={W - R}
                y1={yy}
                y2={yy}
                stroke={f ? "var(--color-grid-minor)" : "var(--color-axis)"}
              />
              {/* Counts: no label where half-way is no whole number. */}
              {Number.isInteger(hi * f) && (
                <text x={L - 8} y={yy + 3.5} textAnchor="end">
                  {num(hi * f)}
                </text>
              )}
            </g>
          );
        })}
        {ticks.map((i) => (
          <text key={i} x={L + slot * (i + 0.5)} y={H - 7} textAnchor="middle">
            {labels[i]}
          </text>
        ))}
        {labels.map((lab, i) => {
          let base = H - B;
          const gap = bw > 5 ? 2 : 0;
          const x0 = L + slot * i + (slot - bw) / 2;
          const present = CLASSES.filter(
            (c) => h(series[c.key]?.[i] ?? 0) >= 1,
          );
          const top = present[present.length - 1]?.key;
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
                    opacity={open && i === n - 1 ? 0.55 : 1}
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
                    bucketName(lab, hourly, open && i === n - 1),
                    CLASSES.map((c) => [c.label, num(series[c.key]?.[i] ?? 0)]),
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
