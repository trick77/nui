import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
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
      exclude: ["src/main.tsx", "src/test/**", "src/**/*.d.ts"],
    },
  },
});
