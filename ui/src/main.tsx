import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { setFetcher } from "./api";
import "./index.css";

async function boot() {
  // npm run dev:fixtures: the API answered from src/fixtures.ts. DEV is
  // false in a production build, so the import and the fixtures drop out.
  if (import.meta.env.DEV && import.meta.env.MODE === "fixtures") {
    const { fixtureFetch } = await import("./fixtures");
    // ?signed-out shows the sign-in card; "fixture-token" signs in.
    const out = new URLSearchParams(window.location.search).has("signed-out");
    setFetcher(fixtureFetch({ delayMs: 250, me: out ? 401 : 200, token: 204 }));
  }
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

void boot();
