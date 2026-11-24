// Entry of /trakk.js. document.currentScript is only set while the script
// runs for the first time, so it is read here, synchronously.
import { start } from "./trakk";

try {
  const w = window as Window & typeof globalThis & { __trakk?: boolean };
  if (!w.__trakk) {
    w.__trakk = true;
    const s = document.currentScript as HTMLScriptElement | null;
    start(w, s ? s.src : null);
  }
} catch {
  // never into the host page
}
