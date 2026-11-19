const BYTES_PER_MB = 1024 * 1024;

export const MIN_USER_QUOTA_BYTES = BYTES_PER_MB;

/**
 * 单用户存储配额（字节）。
 *
 * - 未设置 / 0 / 非法值 → 0，表示不限（保持旧部署行为不变）。
 * - 正数下限 1MB，避免误配一个连缩略图都放不下的值。
 */
export function parseUserQuotaBytes(rawValue: string | undefined | null): number {
  if (!rawValue) return 0;
  const parsed = Number.parseInt(rawValue.trim(), 10);
  if (Number.isNaN(parsed) || parsed <= 0) return 0;
  return Math.max(parsed, MIN_USER_QUOTA_BYTES);
}

/**
 * 解析管理员提交的单账号配额覆盖值。
 *
 * - `null` → 继承全局 `USER_QUOTA_BYTES`
 * - `0` → 该账号不限
 * - 正整数 → 专属上限，低于 1MB 会被抬到 1MB
 * - 其他（负数、小数、字符串等）→ undefined，由调用方返回 400
 */
export function parseUserQuotaOverride(
  value: unknown
): number | null | undefined {
  if (value === null) return null;
  if (typeof value !== "number" || !Number.isInteger(value) || value < 0) {
    return undefined;
  }
  if (value === 0) return 0;
  return Math.max(value, MIN_USER_QUOTA_BYTES);
}

/** 把字节数格式化为便于管理员阅读的单位。 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  if (bytes < 1024) return `${Math.round(bytes)} B`;

  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }

  const rounded = value >= 100 ? Math.round(value) : Number(value.toFixed(2));
  return `${rounded} ${units[unitIndex]}`;
}
