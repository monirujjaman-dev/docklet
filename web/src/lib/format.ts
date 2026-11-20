const units = ["B", "KB", "MB", "GB", "TB"];

export function bytes(n: number, digits = 1): string {
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  if (n < 1) return `${n.toFixed(1)} B`;
  const i = Math.max(0, Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024))));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(digits)} ${units[i]}`;
}

export const rate = (n: number) => `${bytes(n)}/s`;

export const percent = (n: number) =>
  `${n >= 10 ? Math.round(n) : n >= 0.1 || n === 0 ? n.toFixed(1) : n.toFixed(2)}%`;
