/**
 * 复制文本并返回是否真的成功。
 *
 * 只在安全上下文（HTTPS / localhost）使用 Clipboard API；局域网 http 部署下
 * `navigator.clipboard` 要么不存在、要么写入被拒绝，此时回退到 `execCommand`。
 * 调用方必须根据返回值决定是否提示“已复制”，不要吞掉失败。
 */
export async function writeClipboardText(text: string): Promise<boolean> {
  if (
    typeof window !== "undefined" &&
    window.isSecureContext &&
    typeof navigator !== "undefined" &&
    typeof navigator.clipboard?.writeText === "function"
  ) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 落到 execCommand 回退路径。
    }
  }

  if (typeof document === "undefined") return false;

  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.select();
    const copied = document.execCommand("copy");
    document.body.removeChild(textarea);
    return copied;
  } catch {
    return false;
  }
}
