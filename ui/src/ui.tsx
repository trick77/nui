// The page's primitives, in the family's look: tokens only.

import {
  type ReactNode,
  createContext,
  useCallback,
  useContext,
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
import { changeText } from "./format";

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

export const CLASSES: { key: ClassKey; label: string; color: string }[] = [
  { key: "human", label: "Humans", color: "var(--color-s1)" },
  { key: "bot", label: "Known bots", color: "var(--color-s2)" },
  { key: "monitor", label: "Uptime probes", color: "var(--color-s3)" },
  { key: "unknown", label: "Unknown automated", color: "var(--color-s4)" },
];

const CLASS_ONE: Record<ClassKey, string> = {
  human: "Human",
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

export function Panel({
  children,
  className = "",
  id,
  label,
}: {
  children: ReactNode;
  className?: string;
  id?: string;
  label?: string;
}) {
  return (
    <section
      id={id}
      aria-label={label}
      className={`panel scroll-mt-[70px] ${className}`}
    >
      {children}
    </section>
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
}: {
  gen: string | null | undefined;
  text: string;
  className?: string;
}) {
  if (gen)
    return (
      <span className={`gen ${className}`} title="Written by the model">
        {gen}
      </span>
    );
  return <span className={className}>{text}</span>;
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

// A tooltip that follows the pointer, as in the mock: fixed, flipped to
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
