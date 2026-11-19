import { describe, expect, it } from "vitest";
import { findVerificationCode } from "./verificationCode";

// 验证码识别是纯函数,用例只钉住「会不会抓错」:
// 误报会让用户复制到订单号/手机号,比识别不出来更糟。
describe("findVerificationCode", () => {
  it("识别英文关键词后面的验证码", () => {
    expect(findVerificationCode("Your Vercel security code: 349375")).toBe("349375");
  });

  it("识别中文关键词后面的验证码", () => {
    expect(findVerificationCode("您的验证码是 123456")).toBe("123456");
  });

  it("识别中文关键词前面的验证码", () => {
    expect(findVerificationCode("502175 是您的密码重置代码")).toBe("502175");
    expect(findVerificationCode("801202 是您的登录码")).toBe("801202");
  });

  it("支持字母数字混合", () => {
    expect(findVerificationCode("verification code: A1B2C3")).toBe("A1B2C3");
  });

  it("纯字母不当作验证码", () => {
    expect(findVerificationCode("verification code: ABCDEF")).toBeNull();
  });

  it("没有关键词时不抓普通数字", () => {
    expect(findVerificationCode("订单号 202411190001,金额 349375")).toBeNull();
  });

  it("手机号不会被截断成验证码", () => {
    expect(findVerificationCode("验证码 13800138000")).toBeNull();
  });

  it("依次扫描多个来源", () => {
    expect(findVerificationCode("", "verification code: 445566")).toBe("445566");
  });
});
