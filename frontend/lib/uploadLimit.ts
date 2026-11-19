/**
 * 上传上限的展示格式。
 *
 * Go 部署的上限是运行时配置（UPLOAD_MAX_BYTES，可小于 1MiB），由 /api/config 提供；
 * Next 部署仍用构建期 NEXT_PUBLIC_UPLOAD_MAX_BYTES。两种来源都用这一个函数格式化，
 * 避免对小于 1MiB 的值做 MB 取整而显示成 "0MB"。
 */
const BYTES_PER_KB = 1024;
const BYTES_PER_MB = 1024 * 1024;

export function formatUploadMaxLabel(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return "0B";
  }
  if (bytes >= BYTES_PER_MB) {
    const value = bytes / BYTES_PER_MB;
    return `${Number.isInteger(value) ? value : value.toFixed(1)}MB`;
  }
  if (bytes >= BYTES_PER_KB) {
    const value = bytes / BYTES_PER_KB;
    return `${Number.isInteger(value) ? value : value.toFixed(1)}KB`;
  }
  return `${Math.floor(bytes)}B`;
}
