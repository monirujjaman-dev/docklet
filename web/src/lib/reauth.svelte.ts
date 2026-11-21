// Sensitive actions (terminal, file changes) need a login from the last 10
// minutes. When the API says so, ask for the password once and retry.
import { api, ApiError } from "./api";

class Reauth {
  open = $state(false);
  error = $state("");
  private waiters: { ok: () => void; fail: (e: Error) => void }[] = [];

  ask(): Promise<void> {
    this.open = true;
    this.error = "";
    return new Promise((ok, fail) => this.waiters.push({ ok, fail }));
  }

  async submit(password: string) {
    try {
      await api("/auth/reauth", { json: { password } });
      this.open = false;
      this.waiters.splice(0).forEach((w) => w.ok());
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
  }

  cancel() {
    this.open = false;
    this.waiters.splice(0).forEach((w) => w.fail(new Error("Cancelled")));
  }
}

export const reauth = new Reauth();

export function needsReauth(e: unknown) {
  return e instanceof ApiError && !!e.body.reauthRequired;
}

/** Run fn; if it needs a fresh login, ask for the password and run it again. */
export async function withReauth<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    if (!needsReauth(e)) throw e;
    await reauth.ask();
    return await fn();
  }
}
