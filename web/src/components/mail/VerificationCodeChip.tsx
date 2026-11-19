import { Check, Copy, Key } from "@phosphor-icons/react";
import { useState } from "react";

// 验证码快捷复制。列表行里它挂在打开按钮之外(嵌套 button 是非法 HTML,
// 而且点复制不应该顺手把邮件打开)。
export function VerificationCodeChip({ code }: { code: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      // 非安全上下文里剪贴板不可用;复制失败不打断阅读,按钮保持原样。
    }
  };

  return (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-md border border-kumo-line bg-kumo-base px-1.5 py-0.5 text-xs">
      <Key size={12} className="shrink-0 text-kumo-subtle" aria-hidden />
      <span className="text-kumo-subtle">验证码</span>
      <span className="font-medium text-kumo-default tabular-nums">{code}</span>
      <button
        type="button"
        aria-label={copied ? "已复制验证码" : `复制验证码 ${code}`}
        title={copied ? "已复制" : "复制"}
        className="ml-0.5 grid size-5 shrink-0 place-items-center rounded text-kumo-subtle hover:bg-kumo-interact hover:text-kumo-strong"
        onClick={(event) => {
          event.stopPropagation();
          void copy();
        }}
      >
        {copied ? <Check size={12} /> : <Copy size={12} />}
      </button>
    </span>
  );
}
