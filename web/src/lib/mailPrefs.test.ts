import { beforeEach, describe, expect, it } from "vitest";
import { DEFAULT_FOLDER_OPTIONS, normalizeDefaultFolder, setDefaultFolder } from "./mailPrefs";

// 设置弹窗提供的选项就是这两个：垃圾箱与已删除是临时去看的地方，
// 不该成为「每次打开邮箱都先落到这儿」的默认值。
describe("信箱偏好", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("只提供收件箱与已收信两个选项", () => {
    expect(DEFAULT_FOLDER_OPTIONS.map((o) => o.value)).toEqual(["inbox", "all"]);
    expect(DEFAULT_FOLDER_OPTIONS.map((o) => o.label)).toEqual(["收件箱", "已收信"]);
  });

  it("其他分类与损坏的值一律回落到收件箱", () => {
    expect(normalizeDefaultFolder("junkemail")).toBe("inbox");
    expect(normalizeDefaultFolder("deleteditems")).toBe("inbox");
    expect(normalizeDefaultFolder("")).toBe("inbox");
    expect(normalizeDefaultFolder(null)).toBe("inbox");
  });

  it("两个合法值原样保留", () => {
    expect(normalizeDefaultFolder("inbox")).toBe("inbox");
    expect(normalizeDefaultFolder("all")).toBe("all");
  });

  it("写入会落盘到 localStorage", () => {
    setDefaultFolder("all");
    expect(localStorage.getItem("emailbox.mail.defaultFolder")).toBe("all");
  });
});
