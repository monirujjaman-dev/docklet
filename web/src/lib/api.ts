// Typed fetch wrapper for the Docklet API. Adds the CSRF token to
// state-changing requests once a session exists.

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body: Record<string, unknown> = {},
  ) {
    super(message);
  }
}

let csrfToken = "";

export function setCsrfToken(token: string) {
  csrfToken = token;
}

export async function api<T>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const { json, ...rest } = init;
  const method = (rest.method ?? (json !== undefined ? "POST" : "GET")).toUpperCase();
  const headers: Record<string, string> = { Accept: "application/json" };
  if (json !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && method !== "HEAD" && csrfToken) headers["X-CSRF-Token"] = csrfToken;

  const res = await fetch(`/api${path}`, {
    ...rest,
    method,
    credentials: "same-origin",
    headers: { ...headers, ...((rest.headers as Record<string, string>) ?? {}) },
    body: json !== undefined ? JSON.stringify(json) : rest.body,
  });

  let body: Record<string, unknown> = {};
  try {
    body = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    throw new ApiError(res.status, (body.error as string) ?? res.statusText, body);
  }
  return body as T;
}

export type VersionInfo = {
  version: string;
  commit: string;
  date: string;
  go: string;
  os: string;
  arch: string;
};

export type User = { id: number; username: string; totpEnabled: boolean };
export type Me = { user: User; csrfToken: string };

export type AuditEntry = {
  id: number;
  ts: number;
  actor: string;
  action: string;
  target: string;
  ip: string;
  details: Record<string, unknown>;
  hash: string;
};

export type AuditVerify = { ok: boolean; checked: number; brokenAt?: number; reason?: string };
