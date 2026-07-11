// Dex OIDC authentication using authorization code flow with PKCE.
//
// Flow:
// 1. User visits the app → check localStorage for id_token
// 2. No token → redirect to Dex authorization endpoint with code_challenge
// 3. Dex redirects back to /auth/callback with authorization code
// 4. Exchange code + code_verifier for id_token via POST to token endpoint
// 5. Store id_token in localStorage, redirect to /
// 6. WsClient sends the stored id_token in the AUTH frame

const DEX_ISSUER = window.location.port === "5173"
  ? (import.meta.env.VITE_DEX_URL ?? "http://localhost:5556/dex")
  : `${window.location.origin}/dex`;
const CLIENT_ID = "pixeleruv";
const REDIRECT_URI = `${window.location.origin}/auth/callback`;

export function getIdToken(): string | null {
  const tok = localStorage.getItem("id_token");
  return tok === "" ? null : tok;
}

export function getSub(): string | null {
  return localStorage.getItem("sub");
}

export function isLoggedIn(): boolean {
  return getIdToken() !== null;
}

// Generate a random PKCE code verifier (43-128 chars, URL-safe).
function generateCodeVerifier(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return base64url(bytes);
}

// Compute the code challenge (SHA-256 hash of verifier, base64url-encoded).
async function computeCodeChallenge(verifier: string): Promise<string> {
  const data = new TextEncoder().encode(verifier);
  const hash = await crypto.subtle.digest("SHA-256", data);
  return base64url(new Uint8Array(hash));
}

function base64url(bytes: Uint8Array): string {
  let str = "";
  for (const b of bytes) str += String.fromCharCode(b);
  return btoa(str).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// Redirect to Dex login page with PKCE.
export async function redirectToLogin(): Promise<void> {
  const verifier = generateCodeVerifier();
  const challenge = await computeCodeChallenge(verifier);
  const state = crypto.randomUUID();

  sessionStorage.setItem("pkce_verifier", verifier);
  sessionStorage.setItem("oauth_state", state);

  const params = new URLSearchParams({
    client_id: CLIENT_ID,
    redirect_uri: REDIRECT_URI,
    response_type: "code",
    scope: "openid profile email",
    state,
    code_challenge: challenge,
    code_challenge_method: "S256",
  });

  window.location.href = `${DEX_ISSUER}/auth?${params.toString()}`;
}

// Handle the callback from Dex. Exchange the authorization code for an
// id_token, store it, and redirect to /.
export async function handleAuthCallback(): Promise<void> {
  const params = new URLSearchParams(window.location.search);
  const code = params.get("code");
  const state = params.get("state");
  const expectedState = sessionStorage.getItem("oauth_state");
  const verifier = sessionStorage.getItem("pkce_verifier");

  if (!code || !state || state !== expectedState || !verifier) {
    console.error("auth callback: invalid state or missing code/verifier");
    window.location.href = "/";
    return;
  }

  sessionStorage.removeItem("oauth_state");
  sessionStorage.removeItem("pkce_verifier");

  // Exchange the code for an id_token.
  const tokenParams = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    redirect_uri: REDIRECT_URI,
    client_id: CLIENT_ID,
    code_verifier: verifier,
  });

  try {
    const resp = await fetch(`${DEX_ISSUER}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: tokenParams.toString(),
    });
    if (!resp.ok) {
      const body = await resp.text();
      console.error("token exchange failed:", resp.status, body);
      window.location.href = "/";
      return;
    }
    const tokens = await resp.json();
    const idToken = tokens.id_token;
    if (!idToken) {
      console.error("no id_token in token response");
      window.location.href = "/";
      return;
    }

    // Extract sub from the id_token payload.
    try {
      const payload = JSON.parse(atob(idToken.split(".")[1]));
      localStorage.setItem("sub", payload.sub ?? "");
    } catch {
      localStorage.setItem("sub", "");
    }
    localStorage.setItem("id_token", idToken);
  } catch (err) {
    console.error("token exchange error:", err);
  }

  window.location.href = "/";
}

// Clear stored credentials without redirecting. Used when the server
// rejects a stale token (e.g. PB DB was reset) — the WS client clears
// the token so the next reconnect falls back to guest mode.
export function clearIdToken(): void {
  localStorage.removeItem("id_token");
  localStorage.removeItem("sub");
}

export function logout(): void {
  clearIdToken();
  window.location.href = "/";
}
