// Client-side-only display name. Not yet wired into the replication
// protocol or shown as a name tag over avatars — just a local preference
// for now.

const STORAGE_KEY = "display_name";

export function getUsername(): string | null {
  return localStorage.getItem(STORAGE_KEY);
}

export function setUsername(name: string): void {
  localStorage.setItem(STORAGE_KEY, name);
}
