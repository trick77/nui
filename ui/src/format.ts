// Formatting, Swiss conventions (1’234, 22.11.2026) with English labels.
// Formatting only: every number arrives computed.

import type { Change } from "./api";

const LOCALE = "de-CH";

// The separator between two pieces of text: a middle dot with an en space
// each side, plain spaces read cramped in the family's fonts. Built from the
// code point: a raw U+2002 in source trips no-irregular-whitespace and is
// invisible in review. The backend's is apix.Sep.
const ENSP = String.fromCharCode(0x2002);
export const SEP = `${ENSP}·${ENSP}`;
// SEP in two, for a tail that must wrap whole (SepTail): the space a line
// may break at, and the dot that goes with the tail.
export const SEP_BREAK = ENSP;
export const SEP_LEAD = `·${ENSP}`;

// Parts joined by SEP, the empty ones dropped.
export function joinSep(parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(SEP);
}

function upperFirst(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// A network's kind as it stands after its name: ISP, Hosting. The backend's
// word stays lower case: cached readings hang on it.
export function networkKind(kind: string): string {
  return kind === "isp" ? "ISP" : upperFirst(kind);
}

// A signal as shown: No user agent, Hosting network. A first word with a dot
// in it is a name from code (navigator.webdriver) and stays. The backend's
// wording stays too, stored days and cached readings hold it.
export function signal(s: string): string {
  return /^\S*\./.test(s) ? s : upperFirst(s);
}

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

// The date raw events reach back to, as an answer's detail_since has it:
// the server sends one only when what it read from raw begins after the
// range does (on the UTC days it reads), the figures going back further.
// null otherwise. Dates are the viewer's (tz).
export function rawSince(
  detailSince: string | null | undefined,
  tz?: string,
): string | null {
  return detailSince ? date(detailSince, tz) : null;
}

// Day and month, with the year when it is not the current one: a span
// across the new year must not read backwards, nor something a year old
// look current. Years are the viewer's (tz).
export function dayMonth(
  iso: string | null | undefined,
  tz?: string,
  now = new Date(),
): string {
  if (!iso) return "–";
  const full = date(iso, tz);
  if (full.slice(-4) !== date(now.toISOString(), tz).slice(-4)) return full;
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
// today, the date (dayMonth) and time before. Days are the viewer's (tz).
export function when(
  iso: string | null | undefined,
  now = new Date(),
  tz?: string,
): string {
  if (!iso) return "–";
  if (date(iso, tz) === date(now.toISOString(), tz)) return clockSec(iso, tz);
  return dateTime(iso, tz, now);
}

// The date (dayMonth) and time.
export function dateTime(
  iso: string | null | undefined,
  tz?: string,
  now = new Date(),
): string {
  if (!iso) return "–";
  return `${dayMonth(iso, tz, now)} ${clock(iso, tz)}`;
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
