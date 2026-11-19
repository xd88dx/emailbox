import { Checkbox } from "@cloudflare/kumo/components/checkbox";
import { Paperclip } from "@phosphor-icons/react";
import { memo } from "react";
import type { Message } from "@/api/mail";
import { VerificationCodeChip } from "./VerificationCodeChip";
import { findVerificationCode } from "./verificationCode";

export const MESSAGE_ROW_HEIGHT = 76;

interface MessageRowProps {
  message: Message;
  active: boolean;
  checked: boolean;
  onOpen: (message: Message) => void;
  onToggle: (message: Message) => void;
}

// 列表行用 memo:虚拟化下滚动一屏会重渲染十几行,而这些行的数据本身没变。
export const MessageRow = memo(function MessageRow({
  message,
  active,
  checked,
  onOpen,
  onToggle,
}: MessageRowProps) {
  const code = findVerificationCode(message.subject, message.body_preview);
  const open = () => onOpen(message);

  return (
    // 整行都是打开邮件的点击区:行内除了勾选框和复制按钮,
    // 任何空白处(行尾、行间、验证码右侧)都能点开,不会再出现「点了没反应」。
    <div
      role="button"
      tabIndex={0}
      aria-current={active ? "true" : undefined}
      onClick={open}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          open();
        }
      }}
      className={`flex cursor-pointer items-center gap-2 border-b border-kumo-hairline px-3 py-2 text-sm ${
        active ? "bg-ebx-row-active" : "hover:bg-ebx-row-active"
      }`}
      style={{ minHeight: MESSAGE_ROW_HEIGHT }}
    >
      {/* 勾选不该顺手打开邮件。 */}
      <div className="shrink-0" onClick={(event) => event.stopPropagation()}>
        <Checkbox
          checked={checked}
          onCheckedChange={() => onToggle(message)}
          aria-label={`选择邮件 ${message.subject || "(无主题)"}`}
        />
      </div>

      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex items-baseline gap-2">
          <span
            className={`min-w-0 flex-1 truncate ${
              message.is_read ? "text-kumo-subtle" : "font-semibold text-kumo-default"
            }`}
          >
            {message.from || "(未知发件人)"}
          </span>
          <span className="shrink-0 text-xs text-kumo-subtle">
            {formatReceivedAt(message.received_at)}
          </span>
        </div>
        {/* 验证码放在发件人与主题之间,行高随内容增长;没有验证码时整块不渲染。 */}
        {code && (
          <div className="min-w-0">
            <VerificationCodeChip code={code} />
          </div>
        )}
        <div className="flex items-center gap-1">
          <span
            className={`min-w-0 flex-1 truncate ${
              message.is_read ? "text-kumo-default" : "font-medium text-kumo-default"
            }`}
          >
            {message.subject || "(无主题)"}
          </span>
          {message.has_attachments && (
            <Paperclip className="shrink-0 text-kumo-subtle" aria-label="有附件" size={14} />
          )}
        </div>
        <div className="truncate text-xs text-kumo-subtle">{message.body_preview}</div>
      </div>
    </div>
  );
});

// formatReceivedAt 按「离现在多远」选精度:今天的信看时间,今年的看月日,更早的才要年份。
// 邮件列表里绝大多数是最近的信,完整时间戳只会挤掉主题的宽度。
function formatReceivedAt(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";

  const now = new Date();
  const sameDay =
    date.getFullYear() === now.getFullYear() &&
    date.getMonth() === now.getMonth() &&
    date.getDate() === now.getDate();
  if (sameDay) {
    return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
  }
  if (date.getFullYear() === now.getFullYear()) {
    return date.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
  }
  return date.toLocaleDateString("zh-CN", { year: "numeric", month: "numeric", day: "numeric" });
}
