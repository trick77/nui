import { useEffect, useState } from "react";
import { ApiError, api, setAuthListener, type Me } from "./api";
import Dashboard from "./Dashboard";

type Auth =
  | { state: "checking" }
  | { state: "in"; me: Me }
  | { state: "out" }
  | { state: "denied" }
  | { state: "failed"; message: string };

// Sign-in first: /api/me decides. A 401 shows the sign-in link, a 403 the
// no-access screen; neither redirects on its own, so nothing can loop.
export default function App() {
  const [auth, setAuth] = useState<Auth>({ state: "checking" });
  useEffect(() => {
    const ctl = new AbortController();
    api.me(ctl.signal).then(
      (me) => setAuth({ state: "in", me }),
      (e: Error) => {
        if (ctl.signal.aborted) return;
        if (e instanceof ApiError && e.status === 401)
          setAuth({ state: "out" });
        else if (e instanceof ApiError && e.status === 403)
          setAuth({ state: "denied" });
        else setAuth({ state: "failed", message: e.message });
      },
    );
    setAuthListener((s) => setAuth({ state: s === 401 ? "out" : "denied" }));
    return () => {
      ctl.abort();
      setAuthListener(null);
    };
  }, []);

  if (auth.state === "in") return <Dashboard me={auth.me} />;
  return (
    <main className="flex min-h-screen items-center justify-center p-6">
      <div className="panel card w-full max-w-[420px] text-center">
        <h1 className="wordmark m-0 mb-3">nui</h1>
        {auth.state === "checking" && (
          <p className="m-0 text-[13.5px] text-muted" aria-busy="true">
            Checking your session…
          </p>
        )}
        {auth.state === "out" && (
          <>
            <p className="m-0 mb-4 text-[13.5px] text-ink-dim">
              Sign in to see the dashboard.
            </p>
            <a
              href="/api/auth/login"
              className="inline-block rounded-ui-sm bg-accent-fill px-4 py-1.5 text-[14px] text-ink no-underline hover:bg-accent"
            >
              Sign in
            </a>
          </>
        )}
        {auth.state === "denied" && (
          <p role="alert" className="m-0 text-[13.5px] text-ink-dim">
            You are signed in, but not in the group that may see this dashboard.
            Ask the administrator to add you.
          </p>
        )}
        {auth.state === "failed" && (
          <p role="alert" className="m-0 text-[13.5px] text-danger-ink">
            Could not reach the server: {auth.message}
          </p>
        )}
      </div>
    </main>
  );
}
