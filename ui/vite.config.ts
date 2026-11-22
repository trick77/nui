import { build, type Plugin } from "vite";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const outDir = "../backend/web/dist";

// The browser script, built once the app's bundle is written: one classic
// script (a plain <script src defer> cannot load an ES module chunk), no
// dependencies, at the root of the embed directory so the server answers
// /nui.js with it. Part of the one `vite build` the gate runs, not a second
// config: ship owns the build script and tsconfig.node.json.
function tracker(): Plugin {
  return {
    name: "nui-tracker",
    apply: "build",
    async closeBundle() {
      await build({
        configFile: false,
        publicDir: false,
        logLevel: "warn",
        build: {
          outDir,
          emptyOutDir: false,
          // Every browser with fetch keepalive has ES2020; lower would pull
          // in helpers for object spread.
          target: "es2020",
          minify: true,
          lib: {
            entry: "src/tracker/index.ts",
            formats: ["iife"],
            name: "nuiTracker",
            fileName: () => "nui.js",
          },
        },
      });
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), tracker()],
  // Relative, as in ../reworm: the Go server rewrites the shell's "./" refs to
  // /dashboard/ (backend/web), so a deep route still loads /dashboard/assets/.
  // A <base href> would do it in one tag, but kit's CSP sets base-uri 'none'.
  base: "./",
  build: {
    // Straight into the Go embed directory. emptyOutDir stays false so the
    // tracked dist/.gitkeep survives a build: //go:embed all:dist needs a
    // non-empty directory in a fresh clone. ship check (ui.embed) does the
    // stale-asset cleanup instead.
    outDir: "../backend/web/dist",
    emptyOutDir: false,
  },
  server: {
    host: "127.0.0.1",
    proxy: { "/api": "http://127.0.0.1:8080" },
  },
  test: {
    environment: "jsdom",
    // Testing Library registers its automatic cleanup through the global
    // afterEach hook; without globals each render stacks on the previous
    // test's DOM.
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    coverage: {
      provider: "v8",
      reporter: ["text-summary", "lcov"],
      reportsDirectory: "../coverage/ui",
      // Explicit, so an untested module is not silently invisible to the
      // ratio the gate reads.
      include: ["src/**/*.{ts,tsx}"],
      exclude: [
        "src/main.tsx",
        "src/test/**",
        "src/**/*.d.ts",
        "src/**/*.test.{ts,tsx}",
        // The script's entry only hands document.currentScript to start().
        "src/tracker/index.ts",
      ],
    },
  },
});
