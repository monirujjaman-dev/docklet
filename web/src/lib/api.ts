// Minimal typed fetch wrapper for the Docklet API.

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api${path}`, {
    credentials: "same-origin",
    headers: { Accept: "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg);
  }
  return (await res.json()) as T;
}

export type VersionInfo = {
  version: string;
  commit: string;
  date: string;
  go: string;
  os: string;
  arch: string;
};
