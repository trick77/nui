import { build, type InlineConfig, type Plugin } from "vite";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const outDir = "../backend/web/dist";

// The browser script's own property names, mangled in the build like its
// variables. Opt-in by exact name: a property is renamed wherever it is
// read, so none of these may be a browser API's, a key sent to the
// collector or the page's window.nui (event, q, push): the test of the
// built script (src/tracker/build.test.ts) holds them to that.
const TRACKER_PROPS = new RegExp(
  "^(" +
    [
      "batchBytes",
      "batchEvents",
      "quota",
      "urlBytes",
      "strBytes",
      "keyBytes",
      "mapKeys",
      "arrayItems",
      "maxDepth",
      "nodes",
      "propsBytes",
      "ctxBytes",
      "stored",
      "rowBytes",
      "hdrBytes",
      "rowFixed",
      "nameBytes",
      "queueMax",
      "perPage",
      "perDocument",
      "perMinute",
      "flushAfterMs",
      "flushAt",
      "backoffMs",
      "backoffMaxMs",
      "retryMaxMs",
      "failures",
      "sessionMs",
      "sameError",
      "errorKinds",
      "errorKindsPerDocument",
      // Not stop: window.stop() is the browser's.
      "flush",
    ].join("|") +
    ")$",
);

// The browser script, built once the app's bundle is written: one classic
// script (a plain <script src defer> cannot load an ES module chunk), no
// dependencies, at the root of the embed directory so the server answers
// /nui.js with it. Part of the one `vite build` the gate runs, not a second
// config: ship owns the build script and tsconfig.node.json.
// It runs on other people's pages: minified to the property names, no
// comments, no source map. Only the minifier's own renaming, nothing that
// decodes strings or code at run time (pages enforce Trusted Types and a
// CSP; that would only cost size and speed).
function trackerBuild(out: string, write = true): InlineConfig {
  return {
    configFile: false,
    publicDir: false,
    logLevel: "warn",
    // import.meta.env.MODE: test-only code drops out.
    mode: "production",
    build: {
      outDir: out,
      write,
      emptyOutDir: false,
      // Every browser with fetch keepalive has ES2020; lower would pull
      // in helpers for object spread.
      target: "es2020",
      minify: true,
      sourcemap: false,
      lib: {
        entry: "src/tracker/index.ts",
        formats: ["iife"],
        name: "nuiTracker",
        fileName: () => "nui.js",
      },
      rolldownOptions: {
        output: {
          comments: false,
          minify: {
            compress: true,
            mangle: { toplevel: true },
            codegen: { removeWhitespace: true },
            mangleProps: { include: TRACKER_PROPS },
          },
        },
      },
    },
  };
}

function tracker(): Plugin {
  return {
    name: "nui-tracker",
    apply: "build",
    // For the test of the built script.
    api: { build: trackerBuild, props: TRACKER_PROPS },
    async closeBundle() {
      await build(trackerBuild(outDir));
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
    // NUI_API_TARGET: another server (make sim's on NUI_SIM_PORT). Read off
    // globalThis: the config is type-checked without Node's types.
    proxy: {
      "/api":
        (
          globalThis as {
            process?: { env: Record<string, string | undefined> };
          }
        ).process?.env.NUI_API_TARGET ?? "http://127.0.0.1:8080",
    },
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
