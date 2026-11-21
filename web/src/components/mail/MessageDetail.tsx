import { Button } from "@cloudflare/kumo/components/button";
import { ArrowsIn, ArrowsOut, Copy, Translate, X } from "@phosphor-icons/react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  mailApi,
  type Message,
  type MessageDetail as Detail,
  type MessageTranslation,
  type TenantRef,
  mailScopeKey,
} from "@/api/mail";
import { AttachmentList } from "./AttachmentList";
import { MessageBody } from "./MessageBody";
import { VerificationCodeChip } from "./VerificationCodeChip";
import { findVerificationCode } from "./verificationCode";
import { refKey, toRef } from "./messageRef";
import { useAuthStore } from "@/store/authStore";

interface MessageDetailProps {
  tenantID: TenantRef;
  accountID: string;
  // message 是列表行给的摘要。有了它，详情还在路上时头部就能先显示出来，
  // 不至于整栏空白——详情要走一次远端调用，慢的时候是十几秒。
  message: Message;
  onClose: () => void;
  // 详情真正拿到数据、且服务端说这封仍未读时调用一次。
  // 列表层据此调用批量已读接口;放在这里而不是点击时,是为了
  // 详情取失败时不会先把邮件标成已读。
  onLoaded?: (message: Message) => void;
}

interface DetailState {
  key: string;
  detail: Detail | null;
  loading: boolean;
  error: string;
}

const loadingState = (key: string): DetailState => ({
  key,
  detail: null,
  loading: true,
  error: "",
});

// 详情在浏览器里缓存几分钟:在两封信之间来回对比验证码时,
// 每次都重新走一遍上游既慢又白扣取件额度。正文是不可变的,缓存很安全;
// 只保留最近 60 封,避免长时间使用后内存一直涨。
const DETAIL_TTL_MS = 5 * 60 * 1000;
const DETAIL_CACHE_MAX = 60;
const detailCache = new Map<string, { detail: Detail; expiresAt: number }>();

function cachedDetail(key: string): Detail | null {
  const entry = detailCache.get(key);
  if (!entry) return null;
  if (Date.now() > entry.expiresAt) {
    detailCache.delete(key);
    return null;
  }
  return entry.detail;
}

function rememberDetail(key: string, detail: Detail) {
  if (detailCache.size >= DETAIL_CACHE_MAX) {
    const oldest = detailCache.keys().next().value;
    if (oldest) detailCache.delete(oldest);
  }
  detailCache.set(key, { detail, expiresAt: Date.now() + DETAIL_TTL_MS });
}

// 译文缓存十分钟:在两封信之间来回切换不重复调用付费服务。
// 只存内存、不落盘——译文同样是租户的私有数据。
const TRANSLATION_TTL_MS = 10 * 60 * 1000;
const TRANSLATION_CACHE_MAX = 60;
const translationCache = new Map<string, { result: MessageTranslation; expiresAt: number }>();

interface TranslationState {
  key: string;
  result: MessageTranslation | null;
  loading: boolean;
  // 快引擎结果已经显示,大模型还在翻译中。
  upgrading: boolean;
  error: string;
}

const emptyTranslation = (key: string): TranslationState => ({
  key,
  result: null,
  loading: false,
  upgrading: false,
  error: "",
});

function cachedTranslation(key: string): MessageTranslation | null {
  const entry = translationCache.get(key);
  if (!entry) return null;
  if (Date.now() > entry.expiresAt) {
    translationCache.delete(key);
    return null;
  }
  return entry.result;
}

function rememberTranslation(key: string, result: MessageTranslation) {
  if (translationCache.size >= TRANSLATION_CACHE_MAX) {
    const oldest = translationCache.keys().next().value;
    if (oldest) translationCache.delete(oldest);
  }
  translationCache.set(key, { result, expiresAt: Date.now() + TRANSLATION_TTL_MS });
}

export function MessageDetail({
  tenantID,
  accountID,
  message,
  onClose,
  onLoaded,
}: MessageDetailProps) {
  const [fullscreen, setFullscreen] = useState(false);
  const [copied, setCopied] = useState(false);
  const key = `${mailScopeKey(tenantID)}|${accountID}|${refKey(message)}`;
  // 拆成基本类型再进 effect 依赖：直接依赖 message 对象的话，列表刷新后
  // 拿到的是一个内容相同但引用不同的新对象，详情会跟着白拉一次（还要再扣一次配额）。
  const { id: messageID, id_mode: idMode, folder } = toRef(message);

  const [state, setState] = useState(() => loadingState(key));

  // 与 MessageList 同一个套路：换信时在渲染期同步重置，
  // 否则会有一帧把上一封信的正文挂在新主题下面。
  let view = state;
  if (state.key !== key) {
    const cached = cachedDetail(key);
    view = cached ? { key, detail: cached, loading: false, error: "" } : loadingState(key);
    setState(view);
  }

  const translationEnabled = useAuthStore((s) => s.translationEnabled);
  const [translation, setTranslation] = useState(() => {
    const cached = cachedTranslation(key);
    return cached
      ? { key, result: cached, loading: false, upgrading: false, error: "" }
      : emptyTranslation(key);
  });
  let tview = translation;
  if (translation.key !== key) {
    const cached = cachedTranslation(key);
    tview = cached
      ? { key, result: cached, loading: false, upgrading: false, error: "" }
      : emptyTranslation(key);
    setTranslation(tview);
  }

  useEffect(() => {
    if (view.detail) return undefined;
    let ignore = false;
    void (async () => {
      try {
        const resp = await mailApi.message(tenantID, accountID, messageID, {
          folder,
          id_mode: idMode,
        });
        if (ignore) return;
        rememberDetail(key, resp.data);
        setState((prev) =>
          prev.key === key ? { ...prev, detail: resp.data, loading: false } : prev,
        );
      } catch (e) {
        if (ignore) return;
        const error = e instanceof Error ? e.message : "加载失败";
        setState((prev) => (prev.key === key ? { ...prev, error, loading: false } : prev));
      }
    })();
    return () => {
      ignore = true;
    };
  }, [key, tenantID, accountID, messageID, idMode, folder, view.detail]);

  // 详情数据到手后才轮到「置已读」。判断用服务端返回的 is_read,
  // 而不是列表摘要:别的客户端可能已经读过,按摘要判断会多扣一次未读数。
  // 同一封只通知一次——列表更新后本组件不会因此再次触发。
  const notifiedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!view.detail || view.detail.is_read) return;
    if (notifiedRef.current === key) return;
    notifiedRef.current = key;
    onLoaded?.(message);
  }, [view.detail, key, message, onLoaded]);

  // 全屏时 Esc 退出。这是全屏阅读唯一的出口，不给的话用户只能去点右上角。
  useEffect(() => {
    if (!fullscreen) return undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setFullscreen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [fullscreen]);

  const close = useCallback(() => {
    setFullscreen(false);
    onClose();
  }, [onClose]);

  const detail = view.detail;
  // 主题优先,其次正文。正文先转纯文本再找,避免从 HTML 标签/属性里误抓数字。
  const code = useMemo(
    () =>
      detail
        ? findVerificationCode(
            detail.subject,
            message.subject,
            textForCode(detail.body, detail.body_type),
          )
        : findVerificationCode(message.subject),
    [detail, message.subject],
  );

  const copyBody = useCallback(async () => {
    if (!detail) return;
    try {
      await navigator.clipboard.writeText(plainText(detail.body, detail.body_type));
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      // 剪贴板在非安全上下文（http 访问的非 localhost）里不可用。
      // 复制失败不该弹错误对话框打断阅读，按钮维持原样即可。
    }
  }, [detail]);

  // 翻译流的中断句柄。切邮件或关详情时必须取消:否则上一封的大模型请求
  // 会继续占着连接,结果回来还可能写到新邮件上。
  const translateAbort = useRef<AbortController | null>(null);
  useEffect(
    () => () => {
      translateAbort.current?.abort();
    },
    [key],
  );

  const translate = useCallback(async () => {
    translateAbort.current?.abort();
    const controller = new AbortController();
    translateAbort.current = controller;
    setTranslation((prev) =>
      prev.key === key ? { ...prev, loading: true, upgrading: false, error: "" } : prev,
    );
    try {
      await mailApi.translateMessageStream(
        tenantID,
        accountID,
        messageID,
        { folder, id_mode: idMode },
        {
          onResult: (result) => {
            // 只缓存最终稿:快引擎的占位译文下次应当再尝试升级。
            if (result.final) rememberTranslation(key, result);
            setTranslation((prev) =>
              prev.key === key
                ? { key, result, loading: false, upgrading: !result.final, error: "" }
                : prev,
            );
          },
          onError: (message) => {
            setTranslation((prev) =>
              prev.key === key
                ? {
                    ...prev,
                    loading: false,
                    upgrading: false,
                    // 已经有快引擎译文时保留它,不让错误顶掉可读的结果。
                    error: prev.result ? "" : message,
                  }
                : prev,
            );
          },
        },
        controller.signal,
      );
      // 流正常结束:如果最后停在快引擎结果上(大模型失败),就保持那一版。
      setTranslation((prev) =>
        prev.key === key ? { ...prev, loading: false, upgrading: false } : prev,
      );
    } catch (e) {
      if (controller.signal.aborted) return;
      const message = e instanceof Error ? e.message : "翻译失败";
      setTranslation((prev) =>
        prev.key === key
          ? { ...prev, loading: false, upgrading: false, error: prev.result ? "" : message }
          : prev,
      );
    } finally {
      if (translateAbort.current === controller) translateAbort.current = null;
    }
  }, [key, tenantID, accountID, messageID, idMode, folder]);

  return (
    <section
      className={
        fullscreen
          ? "fixed inset-0 z-50 flex flex-col bg-kumo-base"
          : // 详情和邮件列表是并列面板，上边界由外层分隔线提供，
            // 再加一条 border 会变成两条紧挨着的线。
            "flex min-h-0 min-w-0 flex-1 flex-col"
      }
    >
      <header className="flex items-start gap-2 border-b border-kumo-line px-4 py-3">
        <div className="min-w-0 flex-1">
          <h2 className="truncate font-semibold text-kumo-default">
            {message.subject || "(无主题)"}
          </h2>
          <dl className="mt-1 space-y-0.5 text-xs text-kumo-subtle">
            <MetaRow label="发件人" value={message.from} />
            <MetaRow label="收件人" value={detail?.to ?? message.to} />
            <MetaRow label="抄送" value={detail?.cc ?? message.cc} />
            <MetaRow label="时间" value={formatReceivedAt(message.received_at)} />
          </dl>
        </div>
        <Button
          shape="square"
          size="sm"
          variant="ghost"
          icon={fullscreen ? ArrowsIn : ArrowsOut}
          aria-label={fullscreen ? "退出全屏" : "全屏阅读"}
          onClick={() => setFullscreen((v) => !v)}
        />
        <Button
          shape="square"
          size="sm"
          variant="ghost"
          icon={X}
          aria-label="关闭"
          onClick={close}
        />
      </header>

      {/* 操作条。
          参照设计这里还有「回复 / 转发 / 删除 / 标为未读」，我们只放做得到且不会
          制造状态不一致的那些：
          - 回复/转发：没有发信能力，后端只有读取和删除。
          - 删除、标记已读：已经在列表侧（勾选后出现的批量条）里，那条链路会同步更新
            列表的行状态；在详情里再做一份，列表不会跟着变，用户会看到一封「已删除
            但还在列表里」的信。
          - 复制正文：验证码类邮件的高频动作，纯本地操作，没有同步问题。 */}
      {detail && (
        <div className="flex shrink-0 items-center gap-2 border-b border-kumo-line px-4 py-2">
          <Button size="sm" variant="secondary" icon={Copy} onClick={() => void copyBody()}>
            {copied ? "已复制" : "复制正文"}
          </Button>
          {translationEnabled && (
            <Button
              size="sm"
              variant="secondary"
              icon={Translate}
              disabled={tview.loading || tview.upgrading}
              title="正文会发送到部署方配置的翻译服务"
              onClick={() => void translate()}
            >
              {tview.loading || tview.upgrading
                ? "翻译中..."
                : tview.result
                  ? "重新翻译"
                  : "翻译成中文"}
            </Button>
          )}
          {code && <VerificationCodeChip code={code} />}
        </div>
      )}

      {view.loading && <p className="p-6 text-sm text-kumo-subtle">正在加载邮件正文…</p>}
      {view.error && <p className="p-6 text-sm text-kumo-danger">{view.error}</p>}
      {tview.error && <p className="px-4 pt-2 text-sm text-kumo-danger">翻译失败:{tview.error}</p>}

      {tview.result && (
        <div className="max-h-[40%] shrink-0 overflow-y-auto border-b border-kumo-line px-4 py-3">
          <p className="mb-2 text-xs text-kumo-subtle">
            中文译文 · {engineLabel(tview.result.engine)}
            {tview.upgrading ? " · 大模型翻译中..." : ""}
            {tview.result.truncated ? " · 正文过长,仅翻译了前段" : ""}
          </p>
          <p className="whitespace-pre-wrap text-sm leading-relaxed text-kumo-default">
            {tview.result.text}
          </p>
        </div>
      )}

      {detail && (
        <>
          <AttachmentList tenantID={tenantID} accountID={accountID} message={detail} />
          <MessageBody body={detail.body} bodyType={detail.body_type} />
        </>
      )}
    </section>
  );
}

function engineLabel(engine: string): string {
  switch (engine) {
    case "llm":
      return "大模型";
    case "deepl":
      return "DeepL";
    case "libretranslate":
      return "LibreTranslate";
    default:
      return engine;
  }
}

// MetaRow 在字段为空时整行不渲染：大多数邮件没有抄送，
// 留一行空的「抄送：」只会把有用的信息挤下去。
function MetaRow({ label, value }: { label: string; value: string }) {
  if (!value) return null;
  return (
    <div className="flex gap-2">
      <dt className="shrink-0">{label}</dt>
      <dd className="min-w-0 flex-1 truncate text-kumo-default">{value}</dd>
    </div>
  );
}

// textForCode 是只为「找验证码」服务的轻量纯文本化。
// 这里不要求排版准确,把标签换成空格即可;用 DOMParser 会把一份可能
// 几百 KB 的邮件正文重新解析一遍,而这段代码会随每次详情渲染执行。
function textForCode(body: string, bodyType: string): string {
  if (bodyType !== "html") return body;
  return body.replace(/<[^>]*>/g, " ");
}

// plainText 把邮件正文转成可复制的纯文本。
//
// 用 DOMParser 而不是正则剥标签：正则会把 `<div>a</div><div>b</div>` 拼成 "ab"，
// 而验证码邮件里那串数字常常正好被这样粘到相邻文字上。
// DOMParser 解析出的是一棵**游离文档**，不进当前页面、不加载资源、不执行脚本，
// 所以拿未净化的 body 进来也是安全的——这里只读 textContent，从不插入 DOM。
function plainText(body: string, bodyType: string): string {
  if (bodyType !== "html") return body;
  const text = new DOMParser().parseFromString(body, "text/html").body.textContent ?? "";
  // 上游的 HTML 里有大量为了排版塞进来的空行，原样复制出去很难读。
  return text.replace(/\n{3,}/g, "\n\n").trim();
}

// 详情栏和列表行不同，这里空间够，直接给完整时间——
// 判断一封信是不是刚到的验证码，秒级精度是有意义的。
function formatReceivedAt(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", { hour12: false });
}
