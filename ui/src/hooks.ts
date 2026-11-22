import {
  type RefObject,
  useEffect,
  useEffectEvent,
  useLayoutEffect,
  useState,
} from "react";

export type Loaded<T> = {
  data: T | null;
  error: Error | null;
  // True until the answer for the current key is in; data meanwhile is
  // the previous answer, so a refetch does not empty (and re-animate) a
  // panel.
  loading: boolean;
};

// useApi loads once per key (null: nothing to load) and aborts the
// previous request when the key changes.
export function useApi<T>(
  key: string | null,
  load: (signal: AbortSignal) => Promise<T>,
): Loaded<T> {
  const [state, setState] = useState<{
    key: string | null;
    data: T | null;
    error: Error | null;
  }>({ key: null, data: null, error: null });
  const run = useEffectEvent((signal: AbortSignal) => load(signal));
  useEffect(() => {
    if (key == null) return;
    const ctl = new AbortController();
    run(ctl.signal).then(
      (data) => {
        if (!ctl.signal.aborted) setState({ key, data, error: null });
      },
      (error: Error) => {
        if (!ctl.signal.aborted) setState({ key, data: null, error });
      },
    );
    return () => ctl.abort();
  }, [key]);
  return {
    data: state.data,
    error: state.key === key ? state.error : null,
    loading: state.key !== key,
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
// belong to one query (key) and are dropped when it changes.
export function useMore<T>(key: string) {
  const [more, setMore] = useState<{ key: string; items: T[]; next: string }>({
    key: "",
    items: [],
    next: "",
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ key: string; e: Error } | null>(null);
  // Forget the pages when the query changes, also on a way back to it.
  const [seen, setSeen] = useState(key);
  if (seen !== key) {
    setSeen(key);
    setMore({ key: "", items: [], next: "" });
  }
  const mine = more.key === key;
  const load = (fetchPage: () => Promise<{ items: T[]; next: string }>) => {
    setBusy(true);
    setError(null);
    fetchPage().then(
      (p) => {
        setMore((m) => ({
          key,
          items: [...(m.key === key ? m.items : []), ...p.items],
          next: p.next,
        }));
        setBusy(false);
      },
      (e: Error) => {
        setError({ key, e });
        setBusy(false);
      },
    );
  };
  return {
    items: mine ? more.items : [],
    // null: nothing loaded beyond the first page, its own next stands.
    next: mine ? more.next : null,
    busy,
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
