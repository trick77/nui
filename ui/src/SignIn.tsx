import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type AuthMethods } from "./api";

type Methods =
  | { state: "loading" }
  | { state: "ok"; m: AuthMethods }
  | { state: "failed"; message: string };

const PRIMARY =
  "inline-block rounded-ui-sm bg-accent-fill px-4 py-1.5 text-[14px] text-ink no-underline hover:bg-accent";

// What a refused token attempt says. The submitted value is never echoed.
function refusal(res: Response): string {
  switch (res.status) {
    case 400:
      return "The server could not read the sign-in form. Reload the page and try again.";
    case 401:
      return "That is not the admin token.";
    // The CSRF guard: the post did not come from this site's own page.
    case 403:
      return "The server refused a sign-in sent from another site. Open the dashboard at its own address and try again.";
    case 404:
      return "Sign-in with the admin token is not enabled on this server.";
    case 413:
      return "That value is too long to be the admin token.";
    case 429: {
      const s = Number(res.headers.get("Retry-After"));
      return s > 0
        ? `Too many attempts. Wait ${Math.ceil(s)} s and try again.`
        : "Too many attempts. Wait a moment and try again.";
    }
  }
  return "The server could not process the sign-in. Try again.";
}

// The sign-in card's body: the methods this server offers, the single
// sign-on link first, the admin token as the fallback.
export function SignIn({
  onSignedIn,
  lost = false,
}: {
  onSignedIn: () => void;
  // A token sign-in was accepted, yet the session check after it found no
  // session: the browser did not keep the cookie. Said once, never retried.
  lost?: boolean;
}) {
  const [methods, setMethods] = useState<Methods>({ state: "loading" });
  useEffect(() => {
    const ctl = new AbortController();
    api.authMethods(ctl.signal).then(
      (m) => setMethods({ state: "ok", m }),
      (e: Error) => {
        if (!ctl.signal.aborted)
          setMethods({ state: "failed", message: e.message });
      },
    );
    return () => ctl.abort();
  }, []);

  if (methods.state === "loading")
    return (
      <p className="m-0 text-[13.5px] text-muted" aria-busy="true">
        Checking how to sign in…
      </p>
    );
  if (methods.state === "failed")
    return (
      <p role="alert" className="m-0 text-[13.5px] text-danger-ink">
        Could not reach the server: {methods.message}
      </p>
    );
  const { oidc, token } = methods.m;
  if (!oidc && !token)
    return (
      <p role="alert" className="m-0 text-[13.5px] text-ink-dim">
        Sign-in is not configured on this server.
      </p>
    );
  return (
    <>
      {lost && (
        <p role="alert" className="m-0 mb-3 text-[13px] text-danger-ink">
          The token was accepted, but this browser did not keep the session.
          Allow cookies for this site and open it over HTTPS.
        </p>
      )}
      {oidc && (
        <a href="/api/auth/login" className={PRIMARY}>
          Sign in
        </a>
      )}
      {oidc && token && (
        <details className="mt-4 text-left">
          <summary className="cursor-pointer text-center text-[13px] text-muted">
            Use the admin token instead
          </summary>
          <TokenForm primary={false} onSignedIn={onSignedIn} />
        </details>
      )}
      {!oidc && token && <TokenForm primary onSignedIn={onSignedIn} />}
    </>
  );
}

function TokenForm({
  primary,
  onSignedIn,
}: {
  primary: boolean;
  onSignedIn: () => void;
}) {
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (primary) field.current?.focus();
  }, [primary]);

  // The form keeps its method and action: without script it is a plain
  // post. The value lives in the field only until it is sent.
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const sent = value;
    setValue("");
    setError(null);
    setBusy(true);
    try {
      const res = await api.tokenLogin(sent);
      try {
        await res.body?.cancel();
      } catch {
        // nothing to release
      }
      if (res.status === 204) onSignedIn();
      else setError(refusal(res));
    } catch {
      setError("Could not reach the server. Check that it is running.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      method="post"
      action="/api/auth/token"
      aria-label="Admin token sign-in"
      className="mt-3 flex flex-col gap-2.5 text-left"
      onSubmit={(e) => void submit(e)}
    >
      {error && (
        <p role="alert" className="m-0 text-[13px] text-danger-ink">
          {error}
        </p>
      )}
      <label htmlFor="admin-token" className="text-[12.5px] text-muted">
        Admin token
      </label>
      <input
        ref={field}
        id="admin-token"
        name="token"
        type="password"
        autoComplete="current-password"
        required
        className="field w-full"
        value={value}
        onChange={(e) => setValue(e.target.value)}
      />
      <button
        type="submit"
        disabled={busy}
        className={
          primary
            ? `${PRIMARY} cursor-pointer self-center border-0`
            : "cursor-pointer self-center rounded-ui-sm border border-border bg-transparent px-4 py-1.5 text-[13.5px] text-ink hover:bg-active"
        }
      >
        {busy ? "Logging in…" : "Log in"}
      </button>
      <p className="m-0 text-[12px] text-muted">
        This is <code>BACKEND_ADMIN_TOKEN</code> from the service's environment.
      </p>
    </form>
  );
}
