import type { ReactNode } from "react";
import type { ListRow, Watch } from "./api";
import { ms, num, pct } from "./format";
import type { Loaded } from "./hooks";
import { type Col, TopList, changeCol } from "./lists";
import { AllClasses, SectionHead, useDash } from "./ui";
import { ScriptHosts } from "./Watch";

// A site's calls: server requests but 404s and refusals without a route, per endpoint (route and
// operation), account and reason to fail. Always over every class: an API's
// callers are programs, and a website's failure is one whoever called.

const mono = (s: string) => (
  <span className="font-mono text-[12.5px] wrap-anywhere">{s}</span>
);

const failed: Col = {
  head: "Failed",
  num: true,
  cell: (r) => (
    <span className={r.failed ? "text-ochre" : ""}>{pct(r.failed)}</span>
  ),
};
const keyed = (head: string): Col => ({
  head,
  cell: (r: ListRow) => mono(r.label),
});
const calls: Col = { head: "Calls", num: true, cell: (r) => num(r.value) };
const addresses: Col = {
  head: "Addresses",
  num: true,
  cell: (r) => num(r.second),
};

function EndpointsPanel({ head }: { head?: ReactNode }) {
  const { openLog } = useDash();
  return (
    <TopList
      id="endpoints-panel"
      title="Endpoints"
      head={head}
      sources={["server"]}
      tabs={[{ key: "endpoints", label: "Endpoints", dim: "endpoints" }]}
      cols={[
        keyed("Endpoint"),
        calls,
        addresses,
        failed,
        { head: "Median", num: true, cell: (r) => ms(r.p50) },
        { head: "95th", num: true, cell: (r) => ms(r.p95) },
        changeCol,
      ]}
      onRow={(r) => openLog({ endpoint: r.key })}
      empty="No calls in this range."
    />
  );
}

function FailuresPanel({ head }: { head?: ReactNode }) {
  const { openLog } = useDash();
  return (
    <TopList
      id="failures-panel"
      title="Failures"
      head={head}
      sources={["server"]}
      tabs={[{ key: "failures", label: "Failures", dim: "failures" }]}
      cols={[
        keyed("Reason"),
        { ...calls, head: "Count" },
        addresses,
        changeCol,
      ]}
      onRow={(r) => openLog({ failure: r.key })}
      empty="No call failed in this range."
    />
  );
}

// A website's server, beside its pages: its endpoints and why their calls
// fail (a sign-in answered 200 is told by its reason alone). They stand under
// an audience switch they do not follow, and say so.
export function Endpoints() {
  const head = <span>every class of client</span>;
  return (
    <AllClasses>
      <EndpointsPanel head={head} />
      <div className="mt-3">
        <FailuresPanel head={head} />
      </div>
    </AllClasses>
  );
}

function ApiPanels() {
  const { openLog } = useDash();
  return (
    <>
      <EndpointsPanel />
      <div className="mt-3 grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <TopList
          id="accounts-panel"
          title="Accounts"
          sources={["server"]}
          tabs={[{ key: "accounts", label: "Accounts", dim: "accounts" }]}
          cols={[keyed("Account"), calls, addresses, failed, changeCol]}
          onRow={(r) => openLog({ account: r.key, calls: "1" })}
          empty="No call of an account in this range."
        />
        <FailuresPanel />
      </div>
      <div className="mt-3">
        <TopList
          id="agents-panel"
          title="Clients"
          sources={["server"]}
          tabs={[{ key: "agents", label: "Clients", dim: "agents" }]}
          cols={[keyed("Client"), calls, addresses, failed, changeCol]}
          empty="No calls in this range."
        />
      </div>
    </>
  );
}

// What an API has instead of Content. Its pages that run the script report
// their script hosts too: then the card is there, where a new host's
// finding jumps.
export function ApiSection({ watch }: { watch: Loaded<Watch> }) {
  return (
    <AllClasses>
      <SectionHead
        id="content"
        title="API"
        lead="What is called, by which accounts, how long it takes and why it fails. Every class of client; 404s and refusals without a route are no calls."
        sources={["server"]}
      />
      <ApiPanels />
      {!!watch.data?.hosts.length && (
        <div className="mt-3">
          <ScriptHosts watch={watch} />
        </div>
      )}
    </AllClasses>
  );
}
