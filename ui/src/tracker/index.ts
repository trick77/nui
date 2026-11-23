// Entry of /stakeout.js. document.currentScript is only set while the script
// runs for the first time, so it is read here, synchronously.
import { start } from "./stakeout";

try {
  const w = window as Window & typeof globalThis & { __stakeout?: boolean };
  if (!w.__stakeout) {
    w.__stakeout = true;
    const s = document.currentScript as HTMLScriptElement | null;
    start(w, s ? s.src : null);
  }
} catch {
  // never into the host page
}
