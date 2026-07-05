// Dex OIDC authentication for the frontend.
//
// Flow:
// 1. User visits the app → check localStorage for id_token
// 2. No token → redirect to Dex authorization endpoint
// 3. Dex redirects back to /auth/callback with id_token in URL fragment
// 4. Store id_token in localStorage, redirect to /
// 5. WsClient sends the stored id_token in the AUTH frame

// When running in Docker (port 8080), Dex is proxied through nginx at /dex.
// When running in Vite dev (port 5173), Dex is accessed directly on 5556.
const DEX_ISSUER = window.location.port === "5173"
  ? (import.meta.env.VITE_DEX_URL ?? "http://localhost:5556/dex")
  : `${window.location.origin}/dex`;
const CLIENT_ID = "pixeleruv";
const REDIRECT_URI = `${window.location.origin}/auth/callback`;

export function getIdToken(): string | null {
  return localStorage.getItem("id_token");
}

export function getSub(): string | null {
  return localStorage.getItem("sub");
}

export function isLoggedIn(): boolean {
  return getIdToken() !== null;
}

// Redirect to Dex login page.
export function redirectToLogin(): void {
  const state = crypto.randomUUID();
  sessionStorage.setItem("oauth_state", state);

  const params = new URLSearchParams({
    client_id: CLIENT_ID,
    redirect_uri: REDIRECT_URI,
    response_type: "id_token",
    scope: "openid profile email",
    state,
  });

  window.location.href = `${DEX_ISSUER}/auth?${params.toString()}`;
}

// Handle the callback from Dex. Extract id_token from the URL fragment,
// validate state, store the token, and redirect to /.
export function handleAuthCallback(): void {
  const hash = window.location.hash.slice(1);
  const params = new URLSearchParams(hash);

  const idToken = params.get("id_token");
  const state = params.get("state");
  const expectedState = sessionStorage.getItem("oauth_state");

  if (!idToken || !state || state !== expectedState) {
    console.error("auth callback: invalid state or missing token");
    window.location.href = "/";
    return;
  }

  sessionStorage.removeItem("oauth_state");

  // Extract sub from the id_token payload (middle base64 segment).
  try {
    const payload = JSON.parse(atob(idToken.split(".")[1]));
    localStorage.setItem("sub", payload.sub ?? "");
  } catch {
    localStorage.setItem("sub", "");
  }
  localStorage.setItem("id_token", idToken);

  window.location.href = "/";
}

export function logout(): void {
  localStorage.removeItem("id_token");
  localStorage.removeItem("sub");
  window.location.href = "/";
}
