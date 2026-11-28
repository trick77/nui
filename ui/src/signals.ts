// What a visitor's signals mean, each grouped by its own words: which of them
// decided is not stored. The backend's wording (backend/internal/classify)
// stays: stored days and cached readings hold it.

import { signal } from "./format";

const HUMAN = "Human signals";
const BOT = "Bot signals";
// Neither, or not by its words alone: a listed provider may be a bot or an
// uptime probe, "more clients" than the named ones programs or providers.
const OTHER = "Other";

const WORDINGS: [string, RegExp, string][] = [
  [HUMAN, /^signed in$/, "Signed in"],
  [HUMAN, /^script beacon$/, "Uses a real browser"],
  [
    HUMAN,
    /^script beacon from another address$/,
    "Uses a real browser, from another address",
  ],
  [HUMAN, /^engaged$/, "Active on the page"],
  [HUMAN, /^assets loaded$/, "Loads images and styles"],
  [BOT, /^no sign of a browser$/, "No real browser"],
  [BOT, /^no page loaded$/, "Never opened a page"],
  [BOT, /^machine pace$/, "Machine pace"],
  // "client is …" is a listed provider, no program's name starts so.
  [BOT, /^client (?!is )(.+)$/, "Identifier: $1"],
  // The operator's probes file names it (refdata.Own).
  [OTHER, /^own probe (.+)$/, "Own probe: $1"],
  [BOT, /^no user agent$/, "Does not identify itself"],
  [BOT, /^missing browser headers$/, "Missing browser headers"],
  [BOT, /^navigator\.webdriver set$/, "Remote-controlled browser"],
  [BOT, /^claims (.+), address is not \1's$/, "Poses as $1"],
  // Unverified, and from outside the provider's range.
  [BOT, /^claims /, "Claims "],
  [BOT, /^no script beacon$/, "Runs no scripts"],
  [BOT, /^no assets$/, "Loads no images or styles"],
  [BOT, /^mostly refused$/, "Mostly refused"],
  [BOT, /^API only, no page loaded$/, "API calls only"],
  [BOT, /^rapid requests$/, "Rapid requests"],
  [BOT, /^hosting network$/, "From a data center"],
  // The family as the signal of its own says it: inside an "also …" reason
  // there is none.
  [
    BOT,
    /^automation indicators: (.+), hosting network$/,
    "Signs of automation: $1, from a data center",
  ],
  [BOT, /^automation indicators: /, "Signs of automation: "],
];

function word(s: string): { group: string; text: string } {
  const known = WORDINGS.find(([, is]) => is.test(s));
  if (known) return { group: known[0], text: s.replace(known[1], known[2]) };
  // Another part of the address quotes the reason for its class
  // (classify.also).
  const text = s.replace(
    /^(also .+, \d+ of \d+ events \()(.+)\)$/,
    (_, head: string, reason: string) => `${head}${word(reason).text})`,
  );
  return { group: OTHER, text: signal(text) };
}

// The signals shown, in the backend's order. An account says more than a
// blocked script or cached images: signed in hides both.
function shown(signals: string[]): { group: string; text: string }[] {
  const hidden = signals.includes("signed in")
    ? ["no script beacon", "no assets"]
    : [];
  return signals.filter((s) => !hidden.includes(s)).map(word);
}

// The texts in the backend's order: a reason that decided comes first.
export function signalTexts(signals: string[]): string[] {
  return shown(signals).map((w) => w.text);
}

// The groups that have signals, in order.
export function signalGroups(
  signals: string[],
): { heading: string; texts: string[] }[] {
  const all = shown(signals);
  return [HUMAN, BOT, OTHER]
    .map((heading) => ({
      heading,
      texts: all.filter((w) => w.group === heading).map((w) => w.text),
    }))
    .filter((g) => g.texts.length > 0);
}

// A line of a reading's evidence, a signal it cites in its text.
export function evidenceText(e: string): string {
  const s = /^Signal: (.+)$/.exec(e)?.[1];
  return s ? word(s).text : e;
}
