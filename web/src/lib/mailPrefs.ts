import { useSyncExternalStore } from "react";
import type { MailFolder } from "@/api/mail";

// 文件夹的显示名与顺序只在这里维护一份：邮件列表的文件夹标签从这里取，
// 设置里的「信箱偏好」也从这里取标签，避免两处改名只改了一处。
// all 是服务层合成的（收件箱 + 垃圾箱归并），所以叫「已收信」而不是「全部」，
// 免得和下面已读筛选里的「全部」混成同一个概念。
export const FOLDERS: { value: MailFolder; label: string }[] = [
  { value: "inbox", label: "收件箱" },
  { value: "junkemail", label: "垃圾箱" },
  { value: "deleteditems", label: "已删除" },
  { value: "all", label: "已收信" },
];

// 「点开邮箱时停在哪个分类」只允许这两个：垃圾箱和已删除是临时去看的地方，
// 没人想每次进邮箱都先落到那儿。列表里仍然可以切到这两个分类，只是它们当不了默认。
export type DefaultFolder = "inbox" | "all";

export const DEFAULT_FOLDER_OPTIONS: {
  value: DefaultFolder;
  label: string;
  description: string;
}[] = [
  { value: "inbox", label: "收件箱", description: "只看收件箱里的来信" },
  { value: "all", label: "已收信", description: "收件箱与垃圾箱归并显示" },
];

const STORAGE_KEY = "emailbox.mail.defaultFolder";
const FALLBACK: DefaultFolder = "inbox";

// 存进来的值可能是别的分类（旧版本允许选垃圾箱），也可能被手动改坏。
// 不属于这两个的一律回落到收件箱，而不是让列表停在一个当不了默认的分类上。
export function normalizeDefaultFolder(raw: string | null): DefaultFolder {
  return raw === "inbox" || raw === "all" ? raw : FALLBACK;
}

// 默认打开的分类是个很强的个人偏好，记在 localStorage，换标签页/重开页面都沿用。
let cached: DefaultFolder | null = null;
const listeners = new Set<() => void>();

function read(): DefaultFolder {
  if (cached) return cached;
  try {
    cached = normalizeDefaultFolder(localStorage.getItem(STORAGE_KEY));
  } catch {
    // 隐私模式下 localStorage 读不了。用默认值即可，不能因此让列表打不开。
    cached = FALLBACK;
  }
  return cached;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function setDefaultFolder(folder: DefaultFolder) {
  cached = folder;
  try {
    localStorage.setItem(STORAGE_KEY, folder);
  } catch {
    // 同上：记不住偏好无所谓。
  }
  for (const listener of listeners) listener();
}

// 只存「打开时停在哪个分类」这一个偏好。邮件栏里点标签翻分类不改它——
// 用户临时看一眼别的分类，不该把下次打开的位置也一起改掉。
export function useDefaultFolder(): [DefaultFolder, (folder: DefaultFolder) => void] {
  const folder = useSyncExternalStore(subscribe, read, () => FALLBACK);
  return [folder, setDefaultFolder];
}
