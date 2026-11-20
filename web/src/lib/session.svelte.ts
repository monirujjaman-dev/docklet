import { api, ApiError, setCsrfToken, type Me, type User } from "./api";

type Phase = "loading" | "setup" | "login" | "ready" | "error";

class Session {
  phase = $state<Phase>("loading");
  user = $state<User | null>(null);
  error = $state("");

  async init() {
    this.phase = "loading";
    try {
      const { needsSetup } = await api<{ needsSetup: boolean }>("/setup/status");
      if (needsSetup) {
        this.phase = "setup";
        return;
      }
      try {
        this.apply(await api<Me>("/auth/me"));
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) this.phase = "login";
        else throw e;
      }
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      this.phase = "error";
    }
  }

  apply(me: Me) {
    setCsrfToken(me.csrfToken);
    this.user = me.user;
    this.phase = "ready";
  }

  async refresh() {
    this.apply(await api<Me>("/auth/me"));
  }

  async logout() {
    try {
      await api("/auth/logout", { method: "POST" });
    } finally {
      setCsrfToken("");
      this.user = null;
      this.phase = "login";
    }
  }
}

export const session = new Session();
