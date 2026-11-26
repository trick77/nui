import {
  api,
  type Experience,
  type ExperienceRow,
  type ExperienceVital,
  type ListRow,
} from "./api";
import { joinSep, minSec, num, vital } from "./format";
import { useApi } from "./hooks";
import { type Col, SinceNote, TableHead, TableRow } from "./lists";
import {
  CardHead,
  Delta,
  Empty,
  ErrorBox,
  Panel,
  RATING_INK,
  Skeleton,
  VitalInfoButton,
  isVital,
  rating,
  useDash,
  vitalTitle,
} from "./ui";

// What people experienced on a website's pages: their engaged time and the
// web vitals, always people's figures whatever the audience (none under
// Bots, where Content hides it). The ratings are Google's thresholds (ui.tsx).

const missing = <span className="text-muted">–</span>;

// Fewer loads than this rate no page: a p75 of a handful is chance.
const MIN_LOADS = 10;

// A page's vital in its rating's ink, the rating also said in words; muted
// and unrated on fewer than MIN_LOADS loads, a muted dash without a value.
// count: the loads in the title (LCP's cell, whose samples they are).
function rated(
  name: string,
  x: number | null,
  loads: number | null,
  count = false,
) {
  if (x == null) return missing;
  const few = loads != null && loads < MIN_LOADS;
  const r = few ? null : rating(name, x);
  const title = joinSep([
    r,
    few && "too few loads to rate",
    count && loads != null && `${num(loads)} loads`,
  ]);
  return (
    <span
      className={r ? RATING_INK[r] : "text-muted"}
      title={title || undefined}
    >
      {vital(name, x)}
      {r && <span className="sr-only"> {r}</span>}
    </span>
  );
}

// The change, or up from a p75 of 0: no percentage, but worse.
function change(v: ExperienceVital) {
  if (!v.change && v.verdict === "bad")
    return <span className="delta bad">↑ from {vital(v.name, 0)}</span>;
  return <Delta change={v.change} verdict={v.verdict} />;
}

export function ExperiencePanel() {
  const { scope } = useDash();
  const cmp = scope.cmp !== "off";
  // The audience changes nothing: not in the key.
  const e = useApi<Experience>(
    `experience|${scope.site}|${scope.range}|${scope.cmp}`,
    (s) => api.experience(scope, s),
    scope.site,
  );
  const d = e.data;
  // No change to show (no prior figures, a gap in the range): no column of
  // blanks. A vital up from 0 shows one without a change (change()).
  const changes =
    cmp &&
    !!d &&
    (d.engaged_change != null ||
      d.vitals.some((v) => v.change != null || v.verdict === "bad"));
  // Visits without a vital: an older script, which sends none.
  const noVitals = !!d && d.vitals.every((v) => !v.n);
  const cols: Col<ExperienceVital>[] = [
    {
      head: "Vital",
      cell: (v) => (
        <>
          {v.name}
          <VitalInfoButton name={v.name} />
          <span className="ml-2 text-[12px] text-muted max-[600px]:hidden">
            {vitalTitle(v.name)}
          </span>
        </>
      ),
    },
    { head: "75th", num: true, cell: (v) => vital(v.name, v.p75) },
    {
      head: "Rating",
      cell: (v) => {
        const r = rating(v.name, v.p75);
        return r && <span className={RATING_INK[r]}>{r}</span>;
      },
    },
    { head: "Samples", num: true, cell: (v) => num(v.n) },
    ...(changes ? [{ head: "Change", num: true, cell: change }] : []),
  ];
  return (
    <Panel
      id="experience-panel"
      label="Page experience"
      className="card"
      busy={e.loading}
    >
      <CardHead
        title="Page experience"
        right={scope.aud === "all" && <span>people only</span>}
      />
      {e.error ? (
        <ErrorBox error={e.error} />
      ) : !d ? (
        <Skeleton h={220} />
      ) : !d.visits && noVitals ? (
        <Empty>No visits by people in this range yet.</Empty>
      ) : (
        <>
          <p className="mx-0.5 mt-0 mb-2 flex flex-wrap items-baseline gap-x-2.5 text-[13px] text-muted">
            Engaged time per visit
            <b className="text-[15px] font-medium text-ink tabular-nums">
              {minSec(d.engaged_ms)}
            </b>
            {/* The backend sends no verdict with it. */}
            {cmp && <Delta change={d.engaged_change} verdict={null} />}
          </p>
          {noVitals ? (
            <Empty>
              No web vitals in this range: only the current script sends them.
            </Empty>
          ) : (
            <table className="t">
              <TableHead cols={cols} />
              <tbody>
                {d.vitals
                  .filter((v) => isVital(v.name))
                  .map((v) => (
                    <TableRow key={v.name} r={v} cols={cols} />
                  ))}
              </tbody>
            </table>
          )}
        </>
      )}
      <SinceNote of={d} />
    </Panel>
  );
}

// The Pages list's Experience tab: dim=experience's rows (api.ts).
const exp = (r: ListRow) => r as unknown as ExperienceRow;

export const experienceCols: Col[] = [
  {
    head: "Page",
    cell: (r) => (
      <span className="font-mono text-[12.5px] wrap-anywhere">{r.label}</span>
    ),
  },
  { head: "Views", num: true, cell: (r) => num(exp(r).views) },
  {
    head: "Engaged",
    num: true,
    cell: (r) => {
      const x = exp(r).engaged;
      return x == null ? missing : minSec(x);
    },
  },
  {
    head: "LCP",
    num: true,
    cell: (r) => rated("LCP", exp(r).lcp, exp(r).loads, true),
  },
  {
    head: "INP",
    num: true,
    cell: (r) => rated("INP", exp(r).inp, exp(r).loads),
  },
  {
    head: "CLS",
    num: true,
    cell: (r) => rated("CLS", exp(r).cls, exp(r).loads),
  },
];
