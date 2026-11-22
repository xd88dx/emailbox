import { useEffect, useState } from "react";
import type { RefreshStats } from "@/api/jobs";
import { SUPPORT_EMAIL } from "@/lib/support";
import { StatusDot } from "./StatusDot";

// 底部常驻状态条。纯展示，数据由 MailPage 取（和左栏计数是同一份，
// 两处各拉一遍的话，刷新时机不同就会出现左栏和底部数字对不上）。
//
// 数据复用已有的 /mail/refresh/stats——它返回的 total/success/failed/never
// 正好是「账号总数 / 登录成功 / 登录失败 / 尚未登录」，三条聚合 SQL，很便宜。
// 为此再开一个 /mail/stats 端点是重复造一遍。
//
// 参照设计里还有一个「全局未读数」，这里**没有**：邮件不落库，是每次实时从上游拉的，
// 要统计全租户未读就得为每个账号打一次上游请求（还各扣一次 daily_mail_fetch 配额）。
// 未读数只在打开某个账号时才有意义，因此放在邮件列表那一栏里显示。

interface MailStatusBarProps {
  stats: RefreshStats | null;
}

export function MailStatusBar({ stats }: MailStatusBarProps) {
  return (
    <div className="flex shrink-0 flex-col items-center gap-1 border-t border-kumo-line bg-kumo-canvas px-4 py-2 text-xs text-kumo-subtle">
      {/* 三行各自居中：账号统计 / 服务状态 / 客服。窄屏下统计行可换行，
          不再用 h-(--ebx-status-h) 锁高，否则放不下三行。 */}
      <div className="flex max-w-full flex-wrap items-center justify-center gap-x-4 gap-y-1">
        <span className="shrink-0 tabular-nums">{stats?.total ?? 0} 托管账号</span>
        <Stat tone="online" label="凭据正常" value={stats?.success} />
        <Stat tone="error" label="授权失效" value={stats?.failed} />
        <Stat tone="idle" label="未检测" value={stats?.never} />
      </div>

      <div className="flex shrink-0 items-center gap-4">
        <span>服务就绪</span>
        <Clock />
      </div>

      <p className="flex min-w-0 flex-wrap items-center justify-center gap-x-1">
        <span>客服：</span>
        <a
          className="break-all rounded-sm text-kumo-link hover:underline focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-kumo-focus"
          href={`mailto:${SUPPORT_EMAIL}`}
        >
          {SUPPORT_EMAIL}
        </a>
      </p>
    </div>
  );
}

function Stat({
  tone,
  label,
  value,
}: {
  tone: "online" | "error" | "idle";
  label: string;
  value: number | undefined;
}) {
  return (
    <span className="flex shrink-0 items-center gap-1.5 tabular-nums">
      <StatusDot tone={tone} />
      {label} {value ?? 0}
    </span>
  );
}

// 时钟单独成组件，是为了把每秒一次的重渲染关在这一个叶子节点里。
// 放在 MailStatusBar 里会让整条状态条（连带它的统计数字）每秒重渲染一次；
// 再往上放到 MailPage，就是整棵组件树每秒重来一遍。
function Clock() {
  const [now, setNow] = useState(() => new Date());

  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(id);
  }, []);

  return <time className="tabular-nums">{format(now)}</time>;
}

function format(d: Date) {
  const p = (n: number) => String(n).padStart(2, "0");
  // 手写而不用 toLocaleString：后者的输出随浏览器区域设置变化，
  // 状态条宽度会跟着抖，而且不同用户看到的格式不一样。
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
