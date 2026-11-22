// Entry of /nui.js. document.currentScript is only set while the script
// runs for the first time, so it is read here, synchronously.
import { start } from "./nui";

try {
  const w = window as Window & typeof globalThis & { __nui?: boolean };
  if (!w.__nui) {
    w.__nui = true;
    const s = document.currentScript as HTMLScriptElement | null;
    start(w, s ? s.src : null);
  }
} catch {
  // never into the host page
}
