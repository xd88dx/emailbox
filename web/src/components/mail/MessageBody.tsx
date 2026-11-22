import { useMemo, useState } from "react";
import { buildDocument } from "./messageDocument";

interface MessageBodyProps {
  body: string;
  bodyType: "text" | "html";
}

// MessageBody 只负责呈现，净化与文档拼装全在 messageDocument.ts 里，
// 那边是纯函数、可单测。这里唯一的安全职责是 iframe 的 sandbox 属性。
export function MessageBody({ body, bodyType }: MessageBodyProps) {
  const [showRemoteImages, setShowRemoteImages] = useState(false);

  const { html, hadRemoteImages } = useMemo(
    () => buildDocument(body, bodyType, showRemoteImages),
    [body, bodyType, showRemoteImages],
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {hadRemoteImages && !showRemoteImages && (
        <div className="flex flex-wrap items-center gap-3 border-b border-kumo-line bg-kumo-tint px-4 py-2 text-sm">
          <span className="text-kumo-subtle">
            已阻止远程图片。加载它们会让发件人知道你打开了这封邮件。
          </span>
          <button
            type="button"
            className="font-medium text-kumo-link hover:underline"
            onClick={() => setShowRemoteImages(true)}
          >
            仍要加载
          </button>
        </div>
      )}
      <iframe
        // key 让切换图片开关时重建 iframe，而不是复用一个已经加载过远程资源的。
        key={String(showRemoteImages)}
        title="邮件正文"
        // 按设计文档 06-frontend.md：允许链接在新标签打开
        // （allow-popups + allow-popups-to-escape-sandbox 让新标签脱离沙箱，正常显示）。
        // 依然不给 allow-scripts / allow-same-origin：脚本仍跑不起来，
        // iframe 也读不到父页面的 DOM 与存储。链接本身在 messageDocument.ts
        // 里已被加上 target=_blank + rel=noopener。
        sandbox="allow-popups allow-popups-to-escape-sandbox"
        srcDoc={html}
        className="min-h-96 w-full flex-1 border-0 bg-white"
      />
    </div>
  );
}
