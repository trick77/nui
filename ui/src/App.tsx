import { useEffect, useState } from "react";
import { ApiError, api, setAuthListener, type Me } from "./api";
import Dashboard from "./Dashboard";
import { SignIn } from "./SignIn";
import { basePath } from "./state";

type Auth =
  | { state: "checking" }
  | { state: "in"; me: Me }
  | { state: "out" }
  | { state: "denied" }
  | { state: "failed"; message: string };

// Sign-in first: /api/me decides. A 401 shows the sign-in card, a 403 the
// no-access screen; neither redirects on its own, so nothing can loop. A
// token sign-in asks /api/me once more (round).
export default function App() {
  const [auth, setAuth] = useState<Auth>({ state: "checking" });
  const [round, setRound] = useState(0);
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
  }, [round]);

  if (auth.state === "in") return <Dashboard me={auth.me} />;
  // The art only behind sign-in and no access: never fetched while the
  // session is checked, so a signed-in visitor never loads it.
  const art = auth.state === "out" || auth.state === "denied";
  return (
    <main className="flex min-h-screen items-center justify-center p-6">
      {art && (
        <>
          <img
            className="cover"
            src={`${basePath(window.location.pathname)}login-cover.webp`}
            alt=""
            decoding="async"
          />
          <div className="wash" />
        </>
      )}
      <div className="panel card gate w-full max-w-[420px] text-center">
        <h1 className="wordmark m-0 mb-3">nui</h1>
        {auth.state === "checking" && (
          <p className="m-0 text-[13.5px] text-muted" aria-busy="true">
            Checking your session…
          </p>
        )}
        {auth.state === "out" && (
          <>
            <p className="m-0 mb-4 text-[13.5px] text-ink-dim">
              Usage analytics for your own web products.
            </p>
            <SignIn
              onSignedIn={() => {
                setAuth({ state: "checking" });
                setRound((r) => r + 1);
              }}
            />
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
