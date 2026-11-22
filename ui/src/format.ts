// Formatting, Swiss conventions (1’234, 22.11.2026) with English labels.
// Formatting only: every number arrives computed.

import type { Change } from "./api";

const LOCALE = "de-CH";

export function num(x: number | null | undefined, digits = 0): string {
  if (x == null || !Number.isFinite(x)) return "–";
  return x.toLocaleString(LOCALE, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

// A percentage the backend computed (0–100), to a tenth below 10.
export function pct(x: number | null | undefined): string {
  if (x == null || !Number.isFinite(x)) return "–";
  const d = Math.abs(x) < 10 && x !== Math.round(x) ? 1 : 0;
  return `${num(x, d)} %`;
}

// A tile's or row's value in its kind: a rate is a percentage.
export function value(v: number | null | undefined, kind: "sum" | "rate") {
  return kind === "rate" ? pct(v) : num(v);
}

export function bytes(b: number): string {
  if (b < 1024) return `${num(b)} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = b / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${num(v, v < 10 ? 1 : 0)} ${units[i]}`;
}

// The change as the UI writes it: an arrow from its sign, the backend's
// number, "new" when the previous period had none.
export function changeText(c: Change): string {
  if (!c) return "";
  if ("new" in c) return "new";
  const v = "pct" in c ? c.pct : c.pp;
  const unit = "pct" in c ? " %" : " pp";
  const arrow = v > 0 ? "↑" : v < 0 ? "↓" : "→";
  const abs = Math.abs(v);
  return `${arrow} ${num(abs, abs < 10 && abs !== Math.round(abs) ? 1 : 0)}${unit}`;
}

export function date(iso: string | null | undefined, tz?: string): string {
  if (!iso) return "–";
  return new Date(iso).toLocaleDateString(LOCALE, {
    day: "numeric",
    month: "numeric",
    year: "numeric",
    timeZone: tz,
  });
}

export function dayMonth(iso: string | null | undefined, tz?: string): string {
  if (!iso) return "–";
  const d = new Date(iso).toLocaleDateString(LOCALE, {
    day: "numeric",
    month: "numeric",
    timeZone: tz,
  });
  return d.endsWith(".") ? d : `${d}.`;
}

export function clock(iso: string | null | undefined, tz?: string): string {
  if (!iso) return "–";
  return new Date(iso).toLocaleTimeString(LOCALE, {
    hour: "2-digit",
    minute: "2-digit",
    timeZone: tz,
  });
}

export function clockSec(iso: string | null | undefined, tz?: string): string {
  if (!iso) return "–";
  return new Date(iso).toLocaleTimeString(LOCALE, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZone: tz,
  });
}

// A moment relative to now as the log and the trail show it: the time
// today, the date and time before, with the year when it is another one.
// Days and years are the viewer's (tz).
export function when(
  iso: string | null | undefined,
  now = new Date(),
  tz?: string,
): string {
  if (!iso) return "–";
  const d = date(iso, tz);
  const today = date(now.toISOString(), tz);
  if (d === today) return clockSec(iso, tz);
  const year = d.slice(-4) === today.slice(-4);
  return `${year ? dayMonth(iso, tz) : d} ${clock(iso, tz)}`;
}

export function dateTime(iso: string | null | undefined, tz?: string): string {
  if (!iso) return "–";
  return `${dayMonth(iso, tz)} ${clock(iso, tz)}`;
}

// An ISO 3166-1 alpha-2 code as its English name.
let regions: Intl.DisplayNames | null = null;
export function countryName(cc: string): string {
  if (!cc) return "–";
  try {
    regions ??= new Intl.DisplayNames(["en"], { type: "region" });
    return regions.of(cc.toUpperCase()) ?? cc;
  } catch {
    return cc;
  }
}

export function seconds(s: number | null | undefined): string {
  if (s == null) return "–";
  if (s < 60) return `${num(s, s < 10 && s !== Math.round(s) ? 1 : 0)} s`;
  if (s < 3600) return `${num(s / 60, 0)} min`;
  return `${num(s / 3600, 1)} h`;
}
