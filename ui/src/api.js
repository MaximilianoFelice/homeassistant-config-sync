// Thin client for the app's HTTP API. Every URL is relative so it works under
// the Home Assistant ingress prefix. Through ingress no token is needed; for
// direct local use, open the UI with ?token=<api_token> once and it is kept
// for the session.

const TOKEN_KEY = "gcs-token";

function token() {
  const fromUrl = new URLSearchParams(location.search).get("token");
  if (fromUrl) {
    try {
      sessionStorage.setItem(TOKEN_KEY, fromUrl);
    } catch {
      /* storage may be unavailable; keep using the URL */
    }
    return fromUrl;
  }
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

async function request(method, path, body) {
  const headers = { Accept: "application/json" };
  const t = token();
  if (t) headers.Authorization = `Bearer ${t}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(`api/v1/${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    const err = new Error(data?.error ?? `${method} ${path}: HTTP ${res.status}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

export const api = {
  status: () => request("GET", "status"),
  pending: () => request("GET", "pending"),
  conflict: () => request("GET", "conflict"),
  sync: (opts = {}) => request("POST", "sync", opts),
  job: (id) => request("GET", `jobs/${encodeURIComponent(id)}`),
  deployKey: () => request("GET", "deploy-key"),
  rotateKey: () => request("POST", "deploy-key/rotate"),
};

/**
 * Subscribes to server events. EventSource cannot send headers, so with a
 * token (direct local use) it falls back to polling; through ingress it
 * streams. Returns a function that stops listening.
 */
export function onServerEvents(handler, { pollMs = 10000 } = {}) {
  let stopped = false;
  let timer = null;
  let es = null;
  const poll = () => {
    if (stopped) return;
    handler({ type: "poll" });
    timer = setTimeout(poll, pollMs);
  };
  if (!token() && "EventSource" in window) {
    es = new EventSource("api/v1/events");
    es.addEventListener("job", (e) => handler({ type: "job", job: JSON.parse(e.data) }));
    es.onerror = () => {
      es.close();
      es = null;
      if (!timer) timer = setTimeout(poll, pollMs);
    };
  } else {
    timer = setTimeout(poll, pollMs);
  }
  return () => {
    stopped = true;
    es?.close();
    clearTimeout(timer);
  };
}
