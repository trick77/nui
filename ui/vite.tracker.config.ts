import { defineConfig } from "vite";

// The browser script, a second build beside the app: one classic script (a
// plain <script src defer> cannot load an ES module chunk), no
// dependencies, at the root of the embed directory so the server answers
// /nui.js with it.
export default defineConfig({
  publicDir: false,
  build: {
    outDir: "../backend/web/dist",
    emptyOutDir: false,
    // Every browser with fetch keepalive has ES2020; lower would pull in
    // helpers for object spread.
    target: "es2020",
    minify: true,
    lib: {
      entry: "tracker/index.ts",
      formats: ["iife"],
      name: "nuiTracker",
      fileName: () => "nui.js",
    },
  },
});
