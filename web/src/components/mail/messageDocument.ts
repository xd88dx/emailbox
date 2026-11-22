import DOMPurify from "dompurify";

// 邮件正文是本平台**攻击面最大**的一处：内容完全由陌生发件人控制，
// 而这是个多租户 SaaS，一次 XSS 的爆炸半径是整个平台的邮箱凭据。
// 因此做三层，任何一层单独失效都还有另外两层兜着：
//
//   1. DOMPurify 清洗 —— 去掉 script/iframe/事件属性/javascript: URL
//   2. sandbox iframe —— 不给 allow-scripts、不给 allow-same-origin，
//      即使清洗漏了什么，脚本也跑不起来，更读不到父页面的 cookie
//   3. CSP default-src 'none' —— 唯一能挡住「已经在 DOM 里的」外链资源的机制
//
// 第 2、3 层不是「多此一举」：DOMPurify 历史上出过绕过，而这两层是浏览器级别的。
//
// 本文件只做纯字符串处理、不含组件，这样净化逻辑可以直接被单测覆盖
// （见 messageDocument.test.ts，06 文档 §9 把它列为必测项）。

// 远程图片默认阻断，占位用一张 1x1 全透明 GIF。
// 邮件里的 1x1 图片是最常见的阅读追踪手段，加载它等于向发件人确认
// 「这个地址是活的、刚刚有人看了」——对本平台的用户来说，这个确认本身就是要避免的。
// 用真实可解码的图片而不是空 data URI，否则每个被拦下的图都显示成破图图标，
// 看起来像邮件坏了而不是我们有意拦的。
const BLOCKED_IMAGE =
  "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7";

// 唯一允许的 data: 形态——base64 编码的图片。两处都用它，不能各写各的：
// 一处进 DOMPurify 的 ALLOWED_URI_REGEXP，一处在钩子里补 DOMPurify 的一个特例。
const DATA_IMAGE_PATTERN = String.raw`data:image\/(?:png|jpe?g|gif|webp|svg\+xml);base64,`;
const DATA_IMAGE = new RegExp(`^${DATA_IMAGE_PATTERN}`, "i");
const ALLOWED_URI = new RegExp(`^(?:https?|mailto|tel|cid|${DATA_IMAGE_PATTERN})`, "i");

export interface BuiltDocument {
  // html 是可以直接喂给 iframe srcDoc 的完整文档。
  html: string;
  // hadRemoteImages 用来决定要不要显示「已阻止远程图片」那条提示。
  hadRemoteImages: boolean;
}

// buildDocument 把邮件正文变成一个可以安全塞进 sandbox iframe 的完整文档。
export function buildDocument(
  body: string,
  bodyType: "text" | "html",
  showRemoteImages: boolean,
): BuiltDocument {
  if (bodyType !== "html") {
    return { html: wrap(escapeHTML(body), true), hadRemoteImages: false };
  }

  // 邮件自带 <style> 会被 DOMPurify 丢掉：它属于 FORBID_CONTENTS（内容被丢弃），
  // 且以 <style> 开头的字符串会被 HTML 解析器放进 head，默认不序列化。
  // 但 GitHub 这类通知邮件的排版几乎全靠 <style> 里的 CSS 类，直接丢掉就全乱了。
  // 做法：净化前抽出 style，逐条验过「没有外链、没有执行向量」后单独保留，
  // 正文仍走 DOMPurify 完整清洗，两者最后拼进同一个文档。
  const { body: strippedBody, styles } = extractStyles(body);

  let hadRemoteImages = false;
  const purify = DOMPurify();

  // 钩子逐个节点处理外链资源与超链接。放在 DOMPurify 内部做，
  // 而不是清洗后再用正则改 HTML——正则处理 HTML 正是漏洞的常见来源。
  purify.addHook("uponSanitizeElement", (node) => {
    if (!(node instanceof Element)) return;
    if (node.tagName === "IMG") {
      const src = node.getAttribute("src") ?? "";
      if (isRemoteURL(src)) {
        hadRemoteImages = true;
        if (!showRemoteImages) {
          node.setAttribute("src", BLOCKED_IMAGE);
          node.setAttribute("data-blocked-src", src);
        }
      }
    }
  });
  purify.addHook("afterSanitizeAttributes", (node) => {
    if (!(node instanceof Element)) return;

    // DOMPurify 对 img/video/audio/source/track 这几个标签的 src 有个特例：
    // 只要是 data: 开头就直接放行，**根本不过 ALLOWED_URI_REGEXP**
    // （见其 _isValidAttribute 里的 DATA_URI_TAGS 分支）。
    // 也就是说光配 ALLOWED_URI_REGEXP 挡不住 data:text/html 载荷，必须在这里补一刀。
    // 这类 URI 在 <img> 里本来渲染不出东西，但「配置写了却不生效」的差距迟早会被
    // 后来放宽配置的人踩中，不如现在就让实际行为和注释说的一致。
    const src = node.getAttribute("src");
    if (src?.trim().toLowerCase().startsWith("data:") && !DATA_IMAGE.test(src.trim())) {
      node.removeAttribute("src");
    }

    if (node.tagName !== "A") return;
    // sandbox 里点链接默认哪儿也去不了，显式指到新标签页，
    // 并且加 noopener 防止目标页通过 window.opener 操纵本页。
    node.setAttribute("target", "_blank");
    node.setAttribute("rel", "noopener noreferrer nofollow");
  });

  const clean = purify.sanitize(strippedBody, {
    // 前八个 DOMPurify 默认就会去掉，写出来是为了让意图可读，
    // 也防止将来有人放宽配置时不小心把它们放回来。
    //
    // 表单控件是自己加的：DOMPurify 删掉 <form> 时默认**保留子节点**（KEEP_CONTENT），
    // 于是钓鱼邮件里那个「请重新登录」的密码框会原样渲染出来。sandbox 让它提交不出去，
    // 但一个长得像登录框的东西出现在邮件正文里，本身就是在教用户把密码往那儿填。
    FORBID_TAGS: [
      "script",
      "iframe",
      "object",
      "embed",
      "form",
      "base",
      "link",
      "meta",
      "input",
      "button",
      "select",
      "textarea",
    ],
    FORBID_ATTR: ["srcset", "ping", "formaction"],
    // data: URI 只允许图片，别的（比如 data:text/html）等于一个可执行载荷。
    // 注意这一条挡不住 img/video/audio 的 src，那几个要靠上面的钩子。
    ALLOWED_URI_REGEXP: ALLOWED_URI,
  });

  return { html: wrap(clean, false, styles), hadRemoteImages };
}

// extractStyles 把正文里的 <style> 块抽出来，只保留「纯排版」的规则。
// 邮件里的 <style> 是头号 CSS 攻击面：@import 外链样式、url() 外链图片（追踪）、
// expression()/behavior（旧 IE 执行脚本）、@font-face 外链字体都能藏在里面。
// 这些一律不保留，只放行选择器 + 安全声明（颜色/间距/字体/布局等），
// 剩余风险再由 CSP default-src 'none' 兜底。
function extractStyles(html: string): { body: string; styles: string } {
  const blocks: string[] = [];
  const body = html.replace(/<style\b[^>]*>([\s\S]*?)<\/style\s*>/gi, (_m, css: string) => {
    const safe = sanitizeStyleSheet(css);
    if (safe) blocks.push(safe);
    return "";
  });
  return { body, styles: blocks.join("\n") };
}

// 只保留形如 "@media ... { ... }" 或 "selector { decls }" 的结构，
// 每条声明的值再过一遍危险模式。宁可少保留样式，也不放进可执行/外链内容。
function sanitizeStyleSheet(css: string): string {
  // 先去掉注释，避免注释里藏 payload 干扰后续匹配。
  let out = css.replace(/\/\*[\s\S]*?\*\//g, "");
  // @import / @charset / @namespace 是整条外链或编码劫持，直接删掉整行。
  out = out.replace(/@(?:import|charset|namespace)\b[^;]*;/gi, "");
  // @font-face 会拉外链字体，@supports/@media/@keyframes 保留外层但内部同样过滤声明。
  out = out.replace(/@font-face\b[^}]*\}/gi, "");
  // 任何 url() / image-set() / expression() / -moz-binding / behavior 都视为攻击向量。
  if (/\b(?:url|image-set|expression)\s*\(|[-a-z]*binding\s*:|behavior\s*:/i.test(out)) {
    // 不整体放弃（否则一份邮件里有一条坏规则就全丢排版），逐条剥离危险声明。
    out = out.replace(/[^{}]*\b(?:url|image-set|expression)\s*\([^)]*\)[^{};]*;?/gi, "");
    out = out.replace(/[^{}]*(?:[-a-z]*binding|behavior)\s*:[^{};]*;?/gi, "");
  }
  // 二次校验：还有残留（含试图闭合 <style> 逃进文档流的 HTML 标签）就整块丢弃，
  // 绝不带着可疑内容进文档。
  if (
    /<\/?[a-z]|\b(?:url|image-set|expression)\s*\(|[-a-z]*binding\s*:|behavior\s*:|@import|@charset|@namespace/i.test(
      out,
    )
  ) {
    return "";
  }
  return out.trim();
}

function isRemoteURL(src: string): boolean {
  return /^(https?:)?\/\//i.test(src.trim());
}

function escapeHTML(s: string): string {
  return s
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

// wrap 把正文包成一个完整文档。
function wrap(content: string, plainText: boolean, styles = ""): string {
  const csp = [
    "default-src 'none'",
    "img-src data: https: http: cid:",
    "style-src 'unsafe-inline'",
    "font-src data:",
  ].join("; ");

  return `<!doctype html>
<html><head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="${csp}">
<style>
  :root { color-scheme: light; }
  body {
    margin: 0; padding: 16px;
    font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    color: #1f2328; background: #fff;
    word-break: break-word; overflow-wrap: anywhere;
  }
  img { max-width: 100%; height: auto; }
  table { max-width: 100%; }
  pre { white-space: pre-wrap; }
  ${plainText ? "body { white-space: pre-wrap; font-family: ui-monospace, monospace; }" : ""}
  ${styles}
</style>
</head><body>${content}</body></html>`;
}
