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

// A value that follows its input after a pause, for search boxes.
export function useDebounced<T>(v: T, ms: number): T {
  const [d, setD] = useState(v);
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms);
    return () => clearTimeout(t);
  }, [v, ms]);
  return d;
}
