// The page's primitives, in the family's look: tokens only.

import {
  type CSSProperties,
  type ReactNode,
  createContext,
  useCallback,
  useContext,
  useId,
  useState,
} from "react";
import type {
  Change,
  ClassKey,
  LogFilter,
  Panel as PanelKey,
  Scope,
  SiteInfo,
  Verdict,
} from "./api";
import { SEP, SEP_BREAK, SEP_LEAD, changeText } from "./format";

// What every section needs: the query scope, the site and what it has,
// and the page-wide jumps (into the log, to a panel).
export type Dash = {
  scope: Scope;
  site: SiteInfo;
  feed: boolean;
  // Both sources: only then do panels say where their data comes from.
  both: boolean;
  openLog: (f: LogFilter) => void;
  goPanel: (p: PanelKey, filter?: Record<string, string>) => void;
  visitor: string | null;
  selectVisitor: (key: string | null) => void;
};

export const DashContext = createContext<Dash | null>(null);

export function useDash(): Dash {
  const d = useContext(DashContext);
  if (!d) throw new Error("useDash outside the dashboard");
  return d;
}

// A tail after SEP that wraps whole: a narrow column breaks the line before
// the dot, never after it or inside the tail.
export function SepTail({ children }: { children: ReactNode }) {
  return (
    <>
      {SEP_BREAK}
      <span className="whitespace-nowrap">
        {SEP_LEAD}
        {children}
      </span>
    </>
  );
}

// Panels that always show every class, whatever the audience switch
// says: the server's traffic and the errors.
export function AllClasses({ children }: { children: ReactNode }) {
  const d = useDash();
  return (
    <DashContext.Provider value={{ ...d, scope: { ...d.scope, aud: "all" } }}>
      {children}
    </DashContext.Provider>
  );
}

export const CLASSES: { key: ClassKey; label: string; color: string }[] = [
  { key: "human", label: "Humans", color: "var(--color-s1)" },
  { key: "bot", label: "Known bots", color: "var(--color-s2)" },
  { key: "monitor", label: "Uptime probes", color: "var(--color-s3)" },
  { key: "unknown", label: "Unknown automated", color: "var(--color-s4)" },
  // The ramp has four hues; a fifth series is the grey, last in a stack
  // (index.css).
  { key: "api", label: "API clients", color: "var(--color-other)" },
];

const CLASS_ONE: Record<ClassKey, string> = {
  human: "Human",
  api: "API client",
  bot: "Known bot",
  monitor: "Uptime probe",
  unknown: "Unknown automated",
};

export function classColor(k: ClassKey): string {
  return CLASSES.find((c) => c.key === k)?.color ?? "var(--color-other)";
}

export function ClassPill({ cls }: { cls: ClassKey }) {
  return (
    <span className="pill">
      <i style={{ background: classColor(cls) }} />
      {CLASS_ONE[cls] ?? cls}
    </span>
  );
}

// busy: a newer answer is on its way and what shows is the previous one,
// dimmed; the content stays mounted, so nothing re-animates.
export function Panel({
  children,
  className = "",
  id,
  label,
  busy = false,
}: {
  children: ReactNode;
  className?: string;
  id?: string;
  label?: string;
  busy?: boolean;
}) {
  return (
    <section
      id={id}
      aria-label={label}
      aria-busy={busy || undefined}
      className={`panel scroll-mt-[70px] ${className}`}
    >
      {children}
    </section>
  );
}

// The keyboard's way into a clickable table row: its first cell as a
// button, so the row stays a row. Enter and Space click it, and the click
// reaches the row's own handler.
export function RowButton({
  children,
  pressed,
}: {
  children: ReactNode;
  pressed?: boolean;
}) {
  return (
    <button type="button" className="rowbtn" aria-pressed={pressed}>
      {children}
    </button>
  );
}

// The sources a panel's data comes from, shown only on a domain with both.
export function Sources({ of }: { of: ("browser" | "server")[] }) {
  const { both } = useDash();
  if (!both) return null;
  return (
    <>
      {of.map((s) => (
        <span key={s} className={`src ${s === "server" ? "srv" : ""}`}>
          {s}
        </span>
      ))}
    </>
  );
}

export function CardHead({
  title,
  sources,
  right,
  children,
}: {
  title: ReactNode;
  sources?: ("browser" | "server")[];
  right?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="mb-2.5 flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
      <h3 className="m-0 text-[15px] font-medium">{title}</h3>
      {sources && <Sources of={sources} />}
      {children}
      {right && (
        <div className="ml-auto flex flex-wrap items-center gap-3.5 text-[12.5px] text-muted">
          {right}
        </div>
      )}
    </div>
  );
}

export function SectionHead({
  id,
  title,
  lead,
  sources,
}: {
  id: string;
  title: string;
  lead?: ReactNode;
  sources?: ("browser" | "server")[];
}) {
  return (
    <div
      id={id}
      className="mt-[30px] mb-3 flex scroll-mt-[70px] flex-wrap items-baseline gap-3"
    >
      <h2 className="m-0 font-serif text-[21px] font-medium">{title}</h2>
      {lead && <p className="m-0 text-[13px] text-muted">{lead}</p>}
      {sources && <Sources of={sources} />}
    </div>
  );
}

export function Seg<T extends string>({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: { key: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
}) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.key}
          type="button"
          aria-pressed={o.key === value}
          onClick={() => onChange(o.key)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Tabs<T extends string>({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: { key: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
}) {
  return (
    <div className="tabs" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.key}
          type="button"
          aria-pressed={o.key === value}
          onClick={() => onChange(o.key)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

// A change in the backend's verdict colour; the arrow follows the sign.
export function Delta({
  change,
  verdict,
}: {
  change: Change;
  verdict: Verdict | null;
}) {
  if (!change) return null;
  const isNew = "new" in change;
  return (
    <span className={`delta ${isNew ? "new" : (verdict ?? "flat")}`}>
      {changeText(change)}
    </span>
  );
}

// Model text, marked with ✦, or the computed text when there is none.
export function GenText({
  gen,
  text,
  className = "",
  hang = false,
}: {
  gen: string | null | undefined;
  text: string;
  className?: string;
  // A block whose mark hangs in a margin every text of it keeps, marked or
  // not: wrapped lines and unmarked rows start where the marked text does.
  hang?: boolean;
}) {
  const h = hang ? "hang " : "";
  if (gen)
    return (
      <span className={`${h}gen ${className}`} title="Written by the model">
        {gen}
      </span>
    );
  return <span className={`${h}${className}`}>{text}</span>;
}

export function Skeleton({ h, label }: { h: number; label?: string }) {
  return (
    <div
      aria-busy="true"
      aria-label={label ?? "Loading"}
      className="skeleton"
      style={{ height: h }}
    />
  );
}

export function ErrorBox({ error }: { error: Error }) {
  return (
    <div
      role="alert"
      className="rounded-ui border border-danger/40 bg-danger/10 px-4 py-3 text-[13.5px] text-danger-ink"
    >
      Could not load: {error.message}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="m-0 py-3 text-[13px] text-muted">{children}</p>;
}

// Loading, failure, nothing, or the content: the four states of a panel.
export function Loadable<T>({
  data,
  error,
  h,
  children,
}: {
  data: T | null;
  error: Error | null;
  h: number;
  children: (d: T) => ReactNode;
}) {
  if (error) return <ErrorBox error={error} />;
  if (!data) return <Skeleton h={h} />;
  return <>{children(data)}</>;
}

// A tooltip that follows the pointer: fixed, flipped to
// the left near the window's right edge.
export type TipData = {
  x: number;
  y: number;
  title: string;
  rows: [string, string][];
};

export function useTip() {
  const [tip, setTip] = useState<TipData | null>(null);
  const show = useCallback(
    (
      e: { clientX: number; clientY: number },
      title: string,
      rows: [string, string][],
    ) => setTip({ x: e.clientX, y: e.clientY, title, rows }),
    [],
  );
  const hide = useCallback(() => setTip(null), []);
  return { tip, show, hide };
}

export function Tip({ tip }: { tip: TipData | null }) {
  if (!tip) return null;
  const left =
    tip.x + 16 + 240 > (typeof window === "undefined" ? 1e9 : window.innerWidth)
      ? tip.x - 256
      : tip.x + 16;
  return (
    <div
      role="tooltip"
      className="tip"
      style={{ left: Math.max(8, left), top: Math.max(8, tip.y - 20) }}
    >
      <h4>{tip.title}</h4>
      {tip.rows.map(([a, b], i) => (
        <div key={i} className="tr">
          <span>{a}</span>
          {b && <b>{b}</b>}
        </div>
      ))}
    </div>
  );
}

// The web vitals the tracker sends: name, what it measures, Google's
// good and poor thresholds.
const VITALS: Record<string, [string, string, string]> = {
  LCP: [
    "Largest Contentful Paint",
    "Time until the page's largest image or text block shows.",
    "Good up to 2.5 s, poor over 4 s.",
  ],
  CLS: [
    "Cumulative Layout Shift",
    "How much visible content jumped around during the whole visit, shifts right after a click or key press left out. No unit.",
    "Google: good up to 0.1, poor over 0.25, for the worst burst of shifts only; a long visit sums up to more.",
  ],
  INP: [
    "Interaction to Next Paint",
    "The slowest delay from a click, tap or key press to the next screen update.",
    "Good up to 200 ms, poor over 500 ms.",
  ],
  FCP: [
    "First Contentful Paint",
    "Time until the first text or image shows.",
    "Good up to 1.8 s, poor over 3 s.",
  ],
  TTFB: [
    "Time to First Byte",
    "Time until the server's first byte arrived.",
    "Good up to 0.8 s, poor over 1.8 s.",
  ],
};

// An (i) after an event line that names a web vital: a click opens what
// the abbreviation means. A popover, so touch works and Esc or a click
// elsewhere closes it.
export function VitalHint({ kind, what }: { kind: string; what: string }) {
  const id = useId();
  if (kind !== "vital") return null;
  // The backend's shapes only ("vital LCP", "vital LCP 2.1 s", "vital CLS
  // 0.042"): the name is the sender's, "LCP fake" is no vital.
  const name = /^vital (\S+)(?: [\d.]+(?: m?s)?)?$/.exec(what)?.[1] ?? "";
  // The name is the sender's: an inherited key (constructor) is no vital.
  if (!Object.hasOwn(VITALS, name)) return null;
  const v = VITALS[name];
  const pop = `vital${id.replace(/[^\w-]/g, "")}`;
  const anchor = `--${pop}`;
  return (
    <>
      <button
        type="button"
        className="vital-hint"
        popoverTarget={pop}
        aria-label={`What is ${name}?`}
        style={{ anchorName: anchor } as CSSProperties}
      >
        i
      </button>
      {/* Spans only: it sits inside a trail line's span. */}
      <span
        id={pop}
        popover="auto"
        className="tip vital-pop"
        style={{ positionAnchor: anchor } as CSSProperties}
      >
        <span className="vital-pop-title">
          {name}
          {SEP}
          {v[0]}
        </span>
        <span>{v[1]}</span>
        <span>{v[2]}</span>
      </span>
    </>
  );
}
