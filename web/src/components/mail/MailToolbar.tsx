import { Button, LinkButton } from "@cloudflare/kumo/components/button";
import {
  ArrowsClockwise,
  CircleNotch,
  Download,
  EnvelopeOpen,
  Prohibit,
  Trash,
  Upload,
  CheckCircle,
  type IconProps,
} from "@phosphor-icons/react";
import { forwardRef, useEffect, useMemo, useRef, useState } from "react";
import { isTerminal, jobApi } from "@/api/jobs";
import { mailApi, type TenantRef } from "@/api/mail";
import { useAsyncAction } from "@/lib/useAsyncAction";
import { useJobStore } from "@/store/jobStore";
import { useSelectionStore } from "@/store/selectionStore";

// 全局动作条。把原先散在筛选栏（导入/导出/刷新）和 AccountBatchMenu（停用/启用/删除）
// 里的动作收到同一行——那两处一个在列表上方、一个只在有选中时才出现，
// 用户得先知道「选中之后会多出一条」才找得到批量删除。
//
// 这里**只放我们真的做得到的动作**。参照设计里还有「邮件规则 / 新建邮件 / 群发邮件」，
// 那三个后端没有对应能力，摆上去就是三个点了没反应的按钮。
//
// 批量动作常驻但在无选中时禁用，不跟着选中状态出现/消失：
// 让工具条高度恒定，列表不会因为勾了一个复选框就整体往下跳一行。

interface MailToolbarProps {
  tenantID: TenantRef;
  onReload: () => void;
  onExport: () => void;
}

// 用 forwardRef 保持与 Phosphor 图标同一个类型,Kumo 的 icon prop 只接受图标组件。
const SpinningCircle = forwardRef<SVGSVGElement, IconProps>((props, ref) => (
  <CircleNotch
    {...props}
    ref={ref}
    className={["animate-spin", props.className].filter(Boolean).join(" ")}
  />
));
SpinningCircle.displayName = "SpinningCircle";

export function MailToolbar({ tenantID, onReload, onExport }: MailToolbarProps) {
  // 稳定引用：取回 Set 再 useMemo 成数组。直接在选择器里 Array.from
  // 会让 zustand v5 的快照每次都不同，进而无限重渲染（AGENTS.md §6.3）。
  const selected = useSelectionStore((s) => s.selected);
  const ids = useMemo(() => Array.from(selected), [selected]);
  const clear = useSelectionStore((s) => s.clear);
  const { error, pending, run } = useAsyncAction();

  const none = ids.length === 0;

  // 本页提交的任务。jobStore 是全局共享的,只有 jobID 对得上时才把进度
  // 显示在这两个按钮上——否则从 Token 页带回来的刷新任务会串到收信按钮上。
  const job = useJobStore();
  const [ownJob, setOwnJob] = useState<{ id: string; kind: "unread" | "read" } | null>(null);
  const reloadedRef = useRef<string | null>(null);
  const active = Boolean(
    ownJob && job.jobID === ownJob.id && !(job.status && isTerminal(job.status)),
  );

  useEffect(() => {
    if (!ownJob || job.jobID !== ownJob.id) return;
    if (!job.status || !isTerminal(job.status)) return;
    if (reloadedRef.current === ownJob.id) return;
    reloadedRef.current = ownJob.id;
    onReload();
  }, [job.status, job.jobID, ownJob, onReload]);

  const startJob = (id: string, kind: "unread" | "read") => {
    useJobStore.getState().watch(tenantID, id, kind === "unread" ? "unread_check" : "mark_read");
    setOwnJob({ id, kind });
    onReload();
  };

  const submitUnread = () =>
    void run(async () => {
      const resp = await jobApi.submitUnreadCheck(tenantID, { scope: "all" });
      startJob(resp.data.id, "unread");
    });

  const submitMarkRead = () =>
    void run(async () => {
      const resp = await jobApi.submitMarkRead(tenantID, ids);
      startJob(resp.data.id, "read");
      // 不清空选中:标已读往往只是这批账号处理的第一步,清掉后用户要重新勾一遍
      // 才能接着删除/停用;任务结束后列表会重取,选中项按 id 继续保留。
    });

  const stopJob = () =>
    void run(async () => {
      if (ownJob) await jobApi.stop(tenantID, ownJob.id);
    });

  const runningUnread = active && ownJob?.kind === "unread";
  const runningRead = active && ownJob?.kind === "read";
  const refreshPercent =
    job.progress.total > 0 ? Math.min(100, (job.progress.done / job.progress.total) * 100) : 0;
  const readPercent = refreshPercent;
  const act = (fn: () => Promise<unknown>) =>
    void run(async () => {
      await fn();
      clear();
      onReload();
    });

  return (
    <div className="flex h-(--ebx-toolbar-h) shrink-0 items-center gap-2 overflow-x-auto border-b border-kumo-line px-4">
      <LinkButton href="/mail/import" variant="secondary" icon={Upload}>
        导入
      </LinkButton>
      <Button variant="secondary" icon={Download} onClick={onExport}>
        导出
      </Button>
      {/* 进度直接画在按钮上:再放一个「停止」按钮会让工具栏宽度在任务开始/结束时跳一次。
          标签固定为「刷新」,进度只在底边显示,按钮宽度与「导入/导出」完全一致;
          运行中按钮本身就是停止入口。 */}
      <div className="relative shrink-0">
        <Button
          variant="secondary"
          icon={runningUnread ? SpinningCircle : ArrowsClockwise}
          disabled={pending || active}
          onClick={submitUnread}
        >
          刷新
        </Button>
        {runningUnread && (
          <span className="pointer-events-none absolute inset-x-1.5 bottom-1 h-0.5 overflow-hidden rounded-full bg-kumo-tint">
            <span
              className="block h-full bg-kumo-link transition-[width] duration-300"
              style={{ width: `${refreshPercent}%` }}
            />
          </span>
        )}
      </div>

      <span className="mx-2 h-5 w-px shrink-0 bg-kumo-line" aria-hidden />

      {/* 与「刷新」同一套逻辑:进度画在按钮内部,再点一次就是停止。
          标签固定为「已读」,只换图标,宽度不随进度变化,免得整排按钮跟着挪。 */}
      <div className="relative shrink-0">
        <Button
          variant="secondary"
          icon={runningRead ? SpinningCircle : EnvelopeOpen}
          disabled={pending || (active && !runningRead) || (none && !runningRead)}
          onClick={runningRead ? stopJob : submitMarkRead}
        >
          已读
        </Button>
        {runningRead && (
          <span className="pointer-events-none absolute inset-x-1.5 bottom-1 h-0.5 overflow-hidden rounded-full bg-kumo-tint">
            <span
              className="block h-full bg-kumo-link transition-[width] duration-300"
              style={{ width: `${readPercent}%` }}
            />
          </span>
        )}
      </div>
      <Button
        variant="secondary"
        icon={CheckCircle}
        disabled={none || pending}
        onClick={() => act(() => mailApi.batchStatus(tenantID, ids, "active"))}
      >
        启用
      </Button>
      <Button
        variant="secondary"
        icon={Prohibit}
        disabled={none || pending}
        onClick={() => act(() => mailApi.batchStatus(tenantID, ids, "disabled"))}
      >
        停用
      </Button>
      <Button
        variant="secondary-destructive"
        icon={Trash}
        disabled={none || pending}
        onClick={() => act(() => mailApi.batchDelete(tenantID, ids))}
      >
        删除
      </Button>

      {!none && (
        <span className="ml-2 flex shrink-0 items-center gap-2 text-sm text-kumo-subtle">
          已选中 {ids.length} 个
          <button type="button" className="text-kumo-link hover:underline" onClick={clear}>
            取消
          </button>
        </span>
      )}
      {error && <span className="ml-2 shrink-0 text-sm text-kumo-danger">{error}</span>}
    </div>
  );
}
