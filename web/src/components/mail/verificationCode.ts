// 从邮件主题/摘要里挑出验证码,供列表行与详情栏做「一键复制」。
//
// 只认**关键词附近**的 4–8 位字母数字,不做泛化的「提取一串数字」:
// 日期、金额、订单号、手机号同样是一串数字,误报比漏报更让人烦。
// 关键词同时覆盖中英文,并允许验证码出现在关键词之前(中文邮件很常见:
// 「502175 是您的密码重置代码」)。

const KEYWORDS = [
  "密码重置代码",
  "登录验证码",
  "登录码",
  "验证码",
  "校验码",
  "确认码",
  "动态码",
  "安全码",
  "验证代码",
  "激活码",
  "一次性密码",
  "verification code",
  "security code",
  "login code",
  "sign-in code",
  "signin code",
  "confirmation code",
  "access code",
  "one-time password",
  "one-time code",
  "your code",
  "otp",
  "pin",
];

// 4–8 位;纯字母不算(避免把 "code ABCD" 这种占位词当成验证码)。
const TOKEN = "[0-9A-Za-z]{4,8}";

const escapeRegExp = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const KW = KEYWORDS.map(escapeRegExp).join("|");

// 关键词在前:「security code: 349375」「验证码是 123456」
const AFTER = new RegExp(
  `(?:${KW})[\\s,，。:：=]*(?:是|为|is)?[\\s]*(?:code|密码|码)?[\\s:：=]*(?<![0-9A-Za-z])(${TOKEN})(?![0-9A-Za-z])`,
  "i",
);

// 验证码在前:「502175 是您的密码重置代码」「123456 is your verification code」
const BEFORE = new RegExp(
  `\\b(${TOKEN})\\b[\\s,，。]*(?:是|为|is)?[\\s]*(?:您的|你的|your)?[\\s]*(?:${KW})`,
  "i",
);

/**
 * 依次在给出的文本里找验证码,找到第一个就返回;没有则返回 null。
 * 先扫主题再扫摘要/正文,主题里的关键词通常更明确。
 */
export function findVerificationCode(...sources: Array<string | null | undefined>): string | null {
  for (const source of sources) {
    if (!source) continue;
    const text = source.replace(/\s+/g, " ").trim();
    if (!text) continue;
    for (const re of [AFTER, BEFORE]) {
      const match = text.match(re);
      const token = match?.[1];
      if (token && /[0-9]/.test(token)) return token;
    }
  }
  return null;
}
