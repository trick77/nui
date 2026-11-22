import {
  type RefObject,
  useEffect,
  useEffectEvent,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

export type Loaded<T> = {
  data: T | null;
  error: Error | null;
  // True until the answer for the current key is in. Data meanwhile is the
  // previous answer when it has the same owner, so a refetch does not empty
  // (and re-animate) a panel; the panel marks itself busy instead.
  loading: boolean;
};

// useApi loads once per key (null: nothing to load) and aborts the
// previous request when the key changes, so a late answer never lands.
// owner says whose data it is (the site): another owner's answer is never
// handed out for this key, not even while this one loads.
export function useApi<T>(
  key: string | null,
  load: (signal: AbortSignal) => Promise<T>,
  owner?: string,
): Loaded<T> {
  const [state, setState] = useState<{
    key: string | null;
    owner: string | undefined;
    data: T | null;
    error: Error | null;
  }>({ key: null, owner: undefined, data: null, error: null });
  const run = useEffectEvent((signal: AbortSignal) => load(signal));
  useEffect(() => {
    if (key == null) return;
    const ctl = new AbortController();
    run(ctl.signal).then(
      (data) => {
        if (!ctl.signal.aborted) setState({ key, owner, data, error: null });
      },
      (error: Error) => {
        if (!ctl.signal.aborted) setState({ key, owner, data: null, error });
      },
    );
    return () => ctl.abort();
  }, [key, owner]);
  const mine = key != null && state.key === key;
  const kept = key != null && owner != null && state.owner === owner;
  return {
    data: mine || kept ? state.data : null,
    error: mine ? state.error : null,
    loading: key != null && !mine,
  };
}

// The width of an element, followed on resize. jsdom has no layout and no
// ResizeObserver: there the fallback stands.
export function useWidth(
  ref: RefObject<HTMLElement | null>,
  fallback: number,
): number {
  const [w, setW] = useState(fallback);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      const cw = Math.round(entries[0]?.contentRect.width ?? 0);
      if (cw > 0) setW(cw);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return w;
}

// Pages after the first, appended by "Load more" or "Load earlier"; they
// belong to one query (key) and are dropped when it changes. Only the
// latest request of the current query lands: an answer to an older one,
// or to a query since left, is thrown away.
export function useMore<T>(key: string) {
  const [more, setMore] = useState<{ key: string; items: T[]; next: string }>({
    key: "",
    items: [],
    next: "",
  });
  // The query whose next page is loading.
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<{ key: string; e: Error } | null>(null);
  // Forget the pages when the query changes, also on a way back to it.
  const [seen, setSeen] = useState(key);
  if (seen !== key) {
    setSeen(key);
    setMore({ key: "", items: [], next: "" });
    setBusy(null);
  }
  const seq = useRef(0);
  useEffect(() => {
    seq.current++;
  }, [key]);
  const load = (fetchPage: () => Promise<{ items: T[]; next: string }>) => {
    const id = ++seq.current;
    setBusy(key);
    setError(null);
    fetchPage().then(
      (p) => {
        if (id !== seq.current) return;
        setMore((m) => ({
          key,
          items: [...(m.key === key ? m.items : []), ...p.items],
          next: p.next,
        }));
        setBusy(null);
      },
      (e: Error) => {
        if (id !== seq.current) return;
        setError({ key, e });
        setBusy(null);
      },
    );
  };
  const mine = more.key === key;
  return {
    items: mine ? more.items : [],
    // null: nothing loaded beyond the first page, its own next stands.
    next: mine ? more.next : null,
    busy: busy === key,
    error: error?.key === key ? error.e : null,
    load,
  };
}

// A value that follows its input after a pause, for search boxes.
export function useDebounced<T>(v: T, ms: number): T {
  const [d, setD] = useState(v);
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms);
    return () => clearTimeout(t);
  }, [v, ms]);
  return d;
}
