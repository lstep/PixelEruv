// PocketBase authentication using the JS SDK.
//
// Flow:
// 1. User visits the app → check PocketBase authStore for a valid token
// 2. No token → user can register at /register or login at /login
// 3. Login: pb.collection('users').authWithPassword() → token stored in authStore
// 4. Register: pb.collection('users').create() → verification email sent
// 5. WsClient sends the stored token in the AUTH frame
// 6. Pusher validates the token by calling the PocketBase API

import PocketBase from "pocketbase";

// Determine the PocketBase API URL. In dev (vite dev server on :5173),
// PocketBase is at localhost:8090. In Docker (nginx proxy), it's same-origin
// via /api/.
const PB_URL = window.location.port === "5173"
  ? (import.meta.env.VITE_PB_URL ?? "http://localhost:8090")
  : `${window.location.origin}/api`;

export const pb = new PocketBase(PB_URL);

// Auto-cancel any pending realtime subscriptions on page unload.
pb.autoCancellation(false);

export function getIdToken(): string | null {
  const token = pb.authStore.token;
  return token === "" ? null : token;
}

export function getSub(): string | null {
  const record = pb.authStore.record;
  if (!record) return null;
  return record.id;
}

// Device ID is a client-generated UUID stored in localStorage, stable across
// sessions for the same browser. Used as a ban target for guests (alongside
// IP and oidc_sub for logged-in users). Evadable by clearing storage — one
// layer of three.
export function getDeviceId(): string {
  let id = localStorage.getItem("device_id");
  if (!id) {
    id = crypto.randomUUID();
    localStorage.setItem("device_id", id);
  }
  return id;
}

export function isLoggedIn(): boolean {
  return pb.authStore.isValid;
}

// Redirect to the login page.
export function redirectToLogin(): void {
  window.location.href = "/login";
}

// Redirect to the registration page.
export function redirectToRegister(): void {
  window.location.href = "/register";
}

// Register a new user with email and password. After registration,
// PocketBase sends a verification email (if SMTP is configured).
export async function register(
  email: string,
  password: string,
  passwordConfirm: string,
): Promise<void> {
  await pb.collection("users").create({
    email,
    password,
    passwordConfirm,
    emailVisibility: true,
  });
  // Request verification email.
  try {
    await pb.collection("users").requestVerification(email);
  } catch {
    // Non-fatal — the user can request a new verification email later.
  }
}

// Login with email and password. The token is stored in pb.authStore.
export async function login(email: string, password: string): Promise<void> {
  await pb.collection("users").authWithPassword(email, password);
}

// Login with an OAuth2 provider (google, github, facebook). Uses the
// popup-based flow — opens a popup window with the provider's login page.
export async function loginWithProvider(provider: string): Promise<void> {
  await pb.collection("users").authWithOAuth2({ provider });
}

// List configured OAuth2 providers. Returns provider names (e.g. ["google",
// "github"]). Empty array if OAuth2 is not configured.
export async function listAuthProviders(): Promise<
  { name: string; displayName: string }[]
> {
  try {
    const methods = await pb.collection("users").listAuthMethods();
    return (methods.oauth2?.providers ?? []).map((p) => ({
      name: p.name,
      displayName: p.displayName,
    }));
  } catch {
    return [];
  }
}

// Request a verification email for the given email address.
export async function requestVerification(email: string): Promise<void> {
  await pb.collection("users").requestVerification(email);
}

// Confirm email verification from the redirect URL. PocketBase redirects to
// /verify-email?token=... — this method confirms the token.
export async function confirmVerification(token: string): Promise<void> {
  await pb.collection("users").confirmVerification(token);
}

// Clear stored credentials without redirecting. Used when the server
// rejects a stale token (e.g. PB DB was reset) — the WS client clears
// the token so the next reconnect falls back to guest mode.
export function clearIdToken(): void {
  pb.authStore.clear();
}

export function logout(): void {
  clearIdToken();
  window.location.href = "/";
}
