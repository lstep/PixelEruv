// Client-local notification sound preference (localStorage only).
// Per the runtime-options design doc, audio/UI prefs stay client-side —
// the server has no reason to know. Gates the ping notification sound
// and the interaction click SFX in GameScene.

const STORAGE_KEY = "sound_enabled";

// isSoundEnabled returns the user's notification-sound preference. Defaults
// to true when the key is absent (first run / cleared storage).
export function isSoundEnabled(): boolean {
  const v = localStorage.getItem(STORAGE_KEY);
  if (v === null) return true;
  return v === "1";
}

// setSoundEnabled persists the preference. "1"/"0" are used (not "true"/"false")
// so a stray "false" string from an older format doesn't read as truthy.
export function setSoundEnabled(enabled: boolean): void {
  localStorage.setItem(STORAGE_KEY, enabled ? "1" : "0");
}
