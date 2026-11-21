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

// apiDownload POSTs json and saves the response body as a file.
export async function apiDownload(path: string, json: unknown): Promise<string> {
  const res = await fetch(`/api${path}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(csrfToken ? { "X-CSRF-Token": csrfToken } : {}) },
    body: JSON.stringify(json),
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = ((await res.json()) as { error?: string }).error ?? msg;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg, {});
  }
  const name = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "")?.[1] ?? "download";
  const blob = await res.blob(); // fails if the server aborted mid-stream
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
  return name;
}

export type UpdateInfo = {
  current: string;
  latest?: string;
  url?: string;
  publishedAt?: string;
  newer: boolean;
  comparable: boolean;
  inContainer: boolean;
  checkedAt: number;
  error?: string;
  disabled?: boolean;
};

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

export type CaddyStatus = { reachable: boolean; lastSync?: string; lastError?: string };

export type PanelState = {
  domain: string;
  acmeEmail: string;
  status: "none" | "pending" | "active" | "failed";
  message?: string;
  listen: string;
  caddy: CaddyStatus | null;
};

export type AppSummary = {
  id: number;
  projectId: number;
  name: string;
  image: string;
  desiredStatus: "running" | "stopped";
  runtimeStatus: string;
  runtimeMessage?: string;
  domains: string[];
  kind: "web" | "worker";
};

export type Project = { id: number; name: string; createdAt: number; apps: AppSummary[]; databases: DbSummary[] };

export type Deployment = {
  id: number;
  image: string;
  status: "queued" | "running" | "succeeded" | "failed" | "superseded";
  error?: string;
  port: number;
  containerId?: string;
  createdBy: string;
  createdAt: number;
  startedAt?: number;
  finishedAt?: number;
  log?: string;
  current?: boolean;
  source: "image" | "git" | "rollback";
  commitSha?: string;
  commitMessage?: string;
  rollbackOf?: number;
  rollbackable?: boolean;
};

export type GitConfig = {
  url: string;
  branch: string;
  context: string;
  dockerfile: string;
  autoDeploy: boolean;
  deployKey?: string;
  ssh?: boolean;
  webhookPath?: string;
};

export type AppDetail = AppSummary & {
  projectName: string;
  port: number;
  restartPolicy: string;
  command: string;
  cpuMillis: number;
  memoryMb: number;
  env: { key: string; updatedAt: number }[];
  deployments: Deployment[];
  createdAt: number;
  sourceType: "image" | "git";
  git?: GitConfig;
  buildsAvailable: boolean;
};

export type DbSummary = {
  id: number;
  projectId: number;
  name: string;
  engine: string;
  engineLabel: string;
  version: string;
  runtimeStatus: string;
  runtimeMessage?: string;
  publicPort?: number;
};

export type Backup = {
  id: number;
  trigger: "manual" | "schedule" | "pre-restore";
  status: "running" | "succeeded" | "failed";
  sizeBytes: number;
  error?: string;
  startedAt: number;
  finishedAt?: number;
  file?: string;
};

export type DatabaseDetail = DbSummary & {
  projectName: string;
  host: string;
  port: number;
  dbName?: string;
  username?: string;
  memoryMb: number;
  desiredStatus: string;
  backupSchedule: "off" | "hourly" | "daily" | "weekly";
  backupKeep: number;
  defaultEnvKey: string;
  links: { appId: number; appName: string; envKey: string }[];
  apps: { id: number; name: string }[];
  backups: Backup[];
  createdAt: number;
};

export type EngineInfo = { name: string; label: string; versions: string[]; port: number; envKey: string };

export type CronRun = {
  id: number;
  trigger: "schedule" | "manual";
  status: "running" | "succeeded" | "failed" | "timeout" | "skipped";
  exitCode?: number;
  error?: string;
  startedAt: number;
  finishedAt?: number;
  durationMs: number;
  logs?: string;
};

export type CronJob = {
  id: number;
  appId: number;
  name: string;
  schedule: string;
  timezone: string;
  command: string;
  timeoutSec: number;
  concurrency: "skip" | "allow";
  enabled: boolean;
  nextRunAt?: number;
  lastRun?: CronRun;
};
