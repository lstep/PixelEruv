// AuthPage — DOM-based auth pages for registration, login, and email
// verification. Rendered instead of the Phaser game when the URL path
// matches /register, /login, or /verify-email.
//
// Styled to match the welcome page's dark theme.

import {
  register,
  login,
  loginWithProvider,
  listAuthProviders,
  confirmVerification,
  isLoggedIn,
} from "../auth";

const COLORS = {
  bg: "#1a1a2e",
  surface: "#16213e",
  text: "#e0e0e0",
  muted: "#888",
  accent: "#4e9af1",
  error: "#e74c3c",
  success: "#2ecc71",
};

function applyStyles(): void {
  const style = document.createElement("style");
  style.textContent = `
    .auth-page * { margin: 0; padding: 0; box-sizing: border-box; }
    .auth-page {
      font-family: system-ui, sans-serif;
      background: ${COLORS.bg};
      color: ${COLORS.text};
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
    }
    .auth-card {
      background: ${COLORS.surface};
      border-radius: 12px;
      padding: 2rem;
      max-width: 400px;
      width: 100%;
    }
    .auth-card h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
    .auth-card p.subtitle { color: ${COLORS.muted}; font-size: 0.9rem; margin-bottom: 1.5rem; }
    .auth-card label { display: block; color: ${COLORS.muted}; font-size: 0.85rem; margin-bottom: 0.3rem; margin-top: 1rem; }
    .auth-card input {
      width: 100%; padding: 0.6rem 0.8rem; font-size: 0.95rem;
      background: ${COLORS.bg}; color: ${COLORS.text};
      border: 1px solid #444; border-radius: 6px;
    }
    .auth-card input:focus { outline: none; border-color: ${COLORS.accent}; }
    .auth-card button.primary {
      width: 100%; padding: 0.7rem; margin-top: 1.5rem;
      font-size: 1rem; font-weight: 600;
      background: ${COLORS.accent}; color: white;
      border: none; border-radius: 6px; cursor: pointer;
    }
    .auth-card button.primary:hover { opacity: 0.9; }
    .auth-card button.primary:disabled { opacity: 0.5; cursor: default; }
    .auth-card .divider {
      text-align: center; color: ${COLORS.muted}; font-size: 0.85rem;
      margin: 1.5rem 0; position: relative;
    }
    .auth-card .divider::before, .auth-card .divider::after {
      content: ""; position: absolute; top: 50%; width: 40%; height: 1px;
      background: #333;
    }
    .auth-card .divider::before { left: 0; }
    .auth-card .divider::after { right: 0; }
    .auth-card .social-buttons { display: flex; flex-direction: column; gap: 0.5rem; }
    .auth-card button.social {
      padding: 0.6rem; font-size: 0.9rem; font-weight: 500;
      background: ${COLORS.bg}; color: ${COLORS.text};
      border: 1px solid #444; border-radius: 6px; cursor: pointer;
    }
    .auth-card button.social:hover { border-color: ${COLORS.accent}; }
    .auth-card .msg { margin-top: 1rem; font-size: 0.9rem; text-align: center; }
    .auth-card .msg.error { color: ${COLORS.error}; }
    .auth-card .msg.success { color: ${COLORS.success}; }
    .auth-card .link-row { margin-top: 1.5rem; text-align: center; font-size: 0.9rem; color: ${COLORS.muted}; }
    .auth-card .link-row a { color: ${COLORS.accent}; text-decoration: none; }
    .auth-card .link-row a:hover { text-decoration: underline; }
    .auth-card .back-link { display: block; margin-bottom: 1rem; color: ${COLORS.muted}; text-decoration: none; font-size: 0.85rem; }
    .auth-card .back-link:hover { color: ${COLORS.text}; }
  `;
  document.head.appendChild(style);
}

function createPage(): HTMLDivElement {
  applyStyles();
  const page = document.createElement("div");
  page.className = "auth-page";
  document.body.innerHTML = "";
  document.body.appendChild(page);
  return page;
}

function createCard(page: HTMLDivElement, title: string, subtitle: string): HTMLDivElement {
  const card = document.createElement("div");
  card.className = "auth-card";
  page.appendChild(card);

  const back = document.createElement("a");
  back.className = "back-link";
  back.href = "/";
  back.textContent = "← Back to PixelEruv";
  card.appendChild(back);

  const h1 = document.createElement("h1");
  h1.textContent = title;
  card.appendChild(h1);

  const p = document.createElement("p");
  p.className = "subtitle";
  p.textContent = subtitle;
  card.appendChild(p);

  return card;
}

function showMessage(card: HTMLDivElement, text: string, type: "error" | "success"): void {
  const existing = card.querySelector(".msg");
  if (existing) existing.remove();
  const msg = document.createElement("div");
  msg.className = `msg ${type}`;
  msg.textContent = text;
  card.appendChild(msg);
}

async function addSocialButtons(card: HTMLDivElement): Promise<void> {
  const providers = await listAuthProviders();
  if (providers.length === 0) return;

  const divider = document.createElement("div");
  divider.className = "divider";
  divider.textContent = "or";
  card.appendChild(divider);

  const container = document.createElement("div");
  container.className = "social-buttons";
  card.appendChild(container);

  for (const p of providers) {
    const btn = document.createElement("button");
    btn.className = "social";
    btn.textContent = `Continue with ${p.displayName}`;
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      try {
        await loginWithProvider(p.name);
        window.location.href = "/";
      } catch (err) {
        btn.disabled = false;
        showMessage(card, `Login failed: ${(err as Error).message}`, "error");
      }
    });
    container.appendChild(btn);
  }
}

export function renderRegisterPage(): void {
  const page = createPage();
  const card = createCard(page, "Create an account", "Enter your email and choose a password.");

  const emailLabel = document.createElement("label");
  emailLabel.textContent = "Email";
  card.appendChild(emailLabel);
  const emailInput = document.createElement("input");
  emailInput.type = "email";
  emailInput.placeholder = "you@example.com";
  card.appendChild(emailInput);

  const pwLabel = document.createElement("label");
  pwLabel.textContent = "Password";
  card.appendChild(pwLabel);
  const pwInput = document.createElement("input");
  pwInput.type = "password";
  pwInput.placeholder = "At least 8 characters";
  card.appendChild(pwInput);

  const pwConfirmLabel = document.createElement("label");
  pwConfirmLabel.textContent = "Confirm password";
  card.appendChild(pwConfirmLabel);
  const pwConfirmInput = document.createElement("input");
  pwConfirmInput.type = "password";
  card.appendChild(pwConfirmInput);

  const submitBtn = document.createElement("button");
  submitBtn.className = "primary";
  submitBtn.textContent = "Register";
  card.appendChild(submitBtn);

  submitBtn.addEventListener("click", async () => {
    const email = emailInput.value.trim();
    const pw = pwInput.value;
    const pwConfirm = pwConfirmInput.value;

    if (!email || !pw) {
      showMessage(card, "Email and password are required.", "error");
      return;
    }
    if (pw !== pwConfirm) {
      showMessage(card, "Passwords do not match.", "error");
      return;
    }
    if (pw.length < 8) {
      showMessage(card, "Password must be at least 8 characters.", "error");
      return;
    }

    submitBtn.disabled = true;
    try {
      await register(email, pw, pwConfirm);
      showMessage(card, "Account created! Check your email for a verification link.", "success");
      submitBtn.textContent = "Check your email";
    } catch (err) {
      submitBtn.disabled = false;
      const msg = (err as Error).message;
      showMessage(card, `Registration failed: ${msg}`, "error");
    }
  });

  addSocialButtons(card);

  const linkRow = document.createElement("div");
  linkRow.className = "link-row";
  linkRow.innerHTML = `Already have an account? <a href="/login">Log in</a>`;
  card.appendChild(linkRow);
}

export function renderLoginPage(): void {
  if (isLoggedIn()) {
    window.location.href = "/";
    return;
  }

  const page = createPage();
  const card = createCard(page, "Log in", "Enter your email and password.");

  const emailLabel = document.createElement("label");
  emailLabel.textContent = "Email";
  card.appendChild(emailLabel);
  const emailInput = document.createElement("input");
  emailInput.type = "email";
  emailInput.placeholder = "you@example.com";
  card.appendChild(emailInput);

  const pwLabel = document.createElement("label");
  pwLabel.textContent = "Password";
  card.appendChild(pwLabel);
  const pwInput = document.createElement("input");
  pwInput.type = "password";
  card.appendChild(pwInput);

  const submitBtn = document.createElement("button");
  submitBtn.className = "primary";
  submitBtn.textContent = "Log in";
  card.appendChild(submitBtn);

  submitBtn.addEventListener("click", async () => {
    const email = emailInput.value.trim();
    const pw = pwInput.value;
    if (!email || !pw) {
      showMessage(card, "Email and password are required.", "error");
      return;
    }
    submitBtn.disabled = true;
    try {
      await login(email, pw);
      window.location.href = "/";
    } catch (err) {
      submitBtn.disabled = false;
      showMessage(card, `Login failed: ${(err as Error).message}`, "error");
    }
  });

  addSocialButtons(card);

  const linkRow = document.createElement("div");
  linkRow.className = "link-row";
  linkRow.innerHTML = `Don't have an account? <a href="/register">Register</a>`;
  card.appendChild(linkRow);
}

export async function renderVerifyEmailPage(): Promise<void> {
  const page = createPage();
  const card = createCard(page, "Email verification", "Confirming your email address...");

  const params = new URLSearchParams(window.location.search);
  const token = params.get("token");

  if (!token) {
    showMessage(card, "Invalid verification link — no token found.", "error");
    return;
  }

  try {
    await confirmVerification(token);
    showMessage(card, "Email verified! You can now log in.", "success");
    const linkRow = document.createElement("div");
    linkRow.className = "link-row";
    linkRow.innerHTML = `<a href="/login">Go to login</a>`;
    card.appendChild(linkRow);
  } catch (err) {
    showMessage(card, `Verification failed: ${(err as Error).message}`, "error");
  }
}
