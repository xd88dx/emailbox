import { Select } from "@cloudflare/kumo/components/select";
import { Switch } from "@cloudflare/kumo/components/switch";
import { useState } from "react";
import { StatusDot } from "@/components/mail/StatusDot";
import { useAuthStore } from "@/store/authStore";

// 只给几档固定间隔。自动检查会对全部账号打上游,让用户随手填 5 分钟以外的
// 奇怪数字(比如 7)既没有收益,也容易把风控引过来。
const PRESETS = [5, 10, 15, 30, 60];

interface AutoRefreshControlProps {
  // 自动任务正在跑:开关本身仍可操作,只是给出可见的运行提示。
  running: boolean;
  // 用户从关闭切到开启时通知外层立即检查一次。
  onEnabled: () => void;
}

// 设置弹窗里的「自动刷新」一行。偏好存在用户资料里,换设备后仍然生效。
// 原先它常驻在工具栏右侧,现在收进设置弹窗,但仍保留运行状态与间隔选择。
export function AutoRefreshControl({ running, onEnabled }: AutoRefreshControlProps) {
  const user = useAuthStore((s) => s.user);
  const deployDefault = useAuthStore((s) => s.autoRefreshDefaultMinutes);
  const updateAutoRefresh = useAuthStore((s) => s.updateAutoRefresh);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  // null 表示「跟随部署默认」:部署默认关着就是关,开着就是开。
  const enabled = user?.auto_refresh_enabled ?? deployDefault > 0;
  // 关闭时后端仍保留上次的间隔,重新打开沿用;从没选过就用部署默认或 10 分钟。
  const minutes = user?.auto_refresh_minutes ?? (deployDefault > 0 ? deployDefault : 10);
  // 部署默认可能不在预设档位里(比如 20),补进去才不会让 Select 显示空值。
  const options = (
    PRESETS.includes(minutes) ? PRESETS : [...PRESETS, minutes].sort((a, b) => a - b)
  ).map((value) => ({ label: `每 ${value} 分钟`, value: String(value) }));

  async function save(nextEnabled: boolean, nextMinutes: number): Promise<boolean> {
    setSaving(true);
    setError("");
    try {
      await updateAutoRefresh(nextEnabled, nextMinutes);
      return true;
    } catch (e) {
      setError(e instanceof Error ? e.message : "保存失败");
      return false;
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-1.5 text-xs text-kumo-subtle">
          <StatusDot
            tone={enabled ? "online" : "idle"}
            className={running ? "animate-pulse" : undefined}
            label={running ? "自动刷新进行中" : enabled ? "自动刷新已开启" : "自动刷新已关闭"}
          />
          {running ? "自动刷新中" : enabled ? "自动刷新已开启" : "自动刷新已关闭"}
        </span>
        <Switch
          aria-label="自动刷新"
          checked={enabled}
          disabled={saving}
          onCheckedChange={(checked: boolean) =>
            void save(checked, minutes).then((ok) => {
              if (ok && checked) onEnabled();
            })
          }
        />
      </div>
      {enabled && (
        <Select
          className="w-full"
          size="sm"
          aria-label="自动刷新间隔"
          items={options}
          value={String(minutes)}
          disabled={saving}
          onValueChange={(value: string | null) => {
            if (value === null) return;
            void save(true, Number(value));
          }}
        />
      )}
      {error && <p className="text-xs text-kumo-danger">{error}</p>}
    </div>
  );
}
