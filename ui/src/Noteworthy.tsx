import { api, type FindingKind, type Noteworthy as NW } from "./api";
import { dateTime, num } from "./format";
import { useApi } from "./hooks";
import { scopeKey } from "./lists";
import {
  CardHead,
  Empty,
  ErrorBox,
  GenText,
  Panel,
  Skeleton,
  Tip,
  useDash,
  useTip,
} from "./ui";

const KIND: Record<FindingKind, { label: string; color: string }> = {
  problem: { label: "problem", color: "var(--color-danger-ink)" },
  automation: { label: "automation", color: "var(--color-s4)" },
  people: { label: "people", color: "var(--color-s1)" },
};

// The briefing: one headline, at most five one-row findings. The figure
// is computed, the statement the model's (✦) or the computed text; the
// whole line jumps to where the finding leads, the detail is on hover.
export function Noteworthy() {
  const { scope, goPanel } = useDash();
  const { site, range } = scope;
  // The backend compares with the previous period whatever the comparison
  // setting (and for every audience): a change of either is no new query.
  const nw = useApi<NW>(
    scopeKey("noteworthy", { site, range }),
    (s) => api.noteworthy(scope, s),
    site,
  );
  const { tip, show, hide } = useTip();
  return (
    <Panel
      id="noteworthy"
      label="Noteworthy"
      className="card"
      busy={nw.loading}
    >
      <CardHead
        title="Noteworthy"
        right={
          <div className="flex gap-3.5 text-[12px] text-muted">
            {Object.values(KIND).map((k) => (
              <span key={k.label}>
                <i
                  className="mr-1.5 inline-block h-[11px] w-[3px] rounded-[2px] align-[-1px]"
                  style={{ background: k.color }}
                />
                {k.label}
              </span>
            ))}
          </div>
        }
      />
      {nw.error ? (
        <ErrorBox error={nw.error} />
      ) : !nw.data ? (
        <Skeleton h={260} label="Loading the briefing" />
      ) : (
        <>
          <p className="brief">
            <GenText gen={nw.data.headline.gen} text={nw.data.headline.text} />
          </p>
          {nw.data.items.length === 0 ? (
            <Empty>Nothing stands out in this range.</Empty>
          ) : (
            <ul className="nw">
              {nw.data.items.slice(0, 5).map((f, _, shown) => {
                // The lines share the mark's margin when one has the mark.
                const hangItems = shown.some((x) => x.gen);
                // A line over findings whose figures do not add up has
                // none: it shows how many findings it covers.
                const figure = f.figure || num(f.ids.length);
                const unit = f.figure ? f.unit : "findings";
                return (
                  <li key={f.id}>
                    <button
                      type="button"
                      style={{ ["--c" as string]: KIND[f.kind]?.color }}
                      aria-label={`${figure} ${unit}: ${f.gen ?? f.text}`}
                      onClick={() => {
                        hide();
                        goPanel(f.target.panel, f.target.filter);
                      }}
                      onMouseMove={(e) =>
                        show(
                          e,
                          f.gen ? f.text : `${figure} ${unit}`,
                          f.detail.map((d) => [d, ""] as [string, string]),
                        )
                      }
                      onMouseLeave={hide}
                    >
                      <b>{figure}</b>
                      <small>{unit}</small>
                      <span className="say">
                        <GenText hang={hangItems} gen={f.gen} text={f.text} />
                      </span>
                      <em>→</em>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
          <div className="mt-2.5 flex gap-4 px-1 text-[12px] text-muted">
            {nw.data.more > 0 && <span>{num(nw.data.more)} more</span>}
            {nw.data.as_of && (
              <span className="ml-auto">as of {dateTime(nw.data.as_of)}</span>
            )}
          </div>
        </>
      )}
      <Tip tip={tip} />
    </Panel>
  );
}
