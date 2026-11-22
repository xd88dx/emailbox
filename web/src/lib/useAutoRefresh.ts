import { useCallback, useEffect, useRef, useState } from "react";
import { isTerminal, jobApi } from "@/api/jobs";
import { mailScopeKey, type TenantRef } from "@/api/mail";
import { ApiError } from "@/lib/client";

// 自动收信检查的轮询间隔。任务本身可能跑几分钟,3 秒一次只查状态,代价很小。
const POLL_MS = 3000;
// 单轮最多等 30 分钟。任务卡死时不能让定时器永远停在 in-flight,
// 否则之后每一轮都会被 running 挡住,看起来像功能失效。
const MAX_WAIT_MS = 30 * 60_000;

const delay = (ms: number) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));

// useAutoRefresh 定时提交收信检查任务,完成后让调用方重取账号列表。
//
// 它刻意不走 jobStore.watch:那条流是全局单例,watch 新任务会关掉旧 SSE
// 并清空 byAccount,把 Token 页正在看的刷新任务和左栏的检查快照一起冲掉。
// 自动刷新是后台行为,轮询任务状态就够了。
export function useAutoRefresh(
  enabled: boolean,
  minutes: number,
  tenantID: TenantRef,
  onDone: () => void,
  // 用户刚把开关打开时传一个递增的值:本轮立即检查,不必等满一个间隔。
  kick = 0,
) {
  const [running, setRunning] = useState(false);
  const onDoneRef = useRef(onDone);
  const tenantRef = useRef(tenantID);
  const lastKickRef = useRef(0);
  // 在 effect 里同步而不是渲染期写 ref:定时器回调读到的永远是最新值,
  // 又不会让「渲染中修改 ref」触发 react-hooks 规则。
  useEffect(() => {
    onDoneRef.current = onDone;
    tenantRef.current = tenantID;
  });
  const tenantKey = mailScopeKey(tenantID);
  const resetRef = useRef<() => void>(() => {});
  const intervalMs = enabled && minutes > 0 ? minutes * 60_000 : 0;

  useEffect(() => {
    if (intervalMs <= 0 || !tenantKey) return;

    let disposed = false;
    let inFlight = false;
    let timer: number | null = null;
    // 从上一轮结束开始算间隔。任务跑得比间隔还长时,不会出现「刚结束就立刻再来一次」。
    let lastFinished = Date.now();

    const clearTimer = () => {
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    };
    const schedule = (wait: number) => {
      clearTimer();
      if (disposed) return;
      timer = window.setTimeout(() => void run(), Math.max(0, wait));
    };

    async function run() {
      if (disposed || inFlight) return;
      // 后台标签页不发起新的全量检查;切回可见时由 visibilitychange 立即补一轮。
      if (document.visibilityState === "hidden") {
        schedule(intervalMs);
        return;
      }

      inFlight = true;
      setRunning(true);
      try {
        const submitted = await jobApi.submitUnreadCheck(tenantRef.current, { scope: "all" });
        const jobID = submitted.data.id;
        const deadline = Date.now() + MAX_WAIT_MS;
        while (!disposed && Date.now() < deadline) {
          await delay(POLL_MS);
          if (disposed) return;
          const job = await jobApi.get(tenantRef.current, jobID);
          if (isTerminal(job.data.status)) break;
        }
        if (!disposed) onDoneRef.current();
      } catch (e) {
        // 409 是手动刷新/批量已读正在跑,自动这一轮让路;下一轮再试,不打扰用户。
        // 401/403 说明会话或账号状态已经变了,继续排程只会重复撞墙。
        if (e instanceof ApiError && (e.status === 401 || e.status === 403)) disposed = true;
      } finally {
        inFlight = false;
        // 即使 disposed 也要收掉 running:否则用户在中途关掉开关后,
        // 工具栏的「刷新/已读」会一直因为 autoRunning 被禁用。
        setRunning(false);
        if (!disposed) {
          lastFinished = Date.now();
          schedule(intervalMs);
        }
      }
    }

    const onVisibility = () => {
      if (document.visibilityState !== "visible" || inFlight) return;
      const elapsed = Date.now() - lastFinished;
      schedule(elapsed >= intervalMs ? 0 : intervalMs - elapsed);
    };

    // 手动刷新已经拿到最新状态,把计时器从现在重新开始,避免紧接着又跑一轮。
    resetRef.current = () => {
      lastFinished = Date.now();
      schedule(intervalMs);
    };

    // 开关被用户打开的那一轮直接开跑;其余情况(首次进入、改间隔)照常等一个间隔。
    const immediate = kick !== lastKickRef.current;
    lastKickRef.current = kick;
    schedule(immediate ? 0 : intervalMs);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      disposed = true;
      clearTimer();
      resetRef.current = () => {};
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [intervalMs, tenantKey, kick]);

  const resetTimer = useCallback(() => resetRef.current(), []);
  return { running, resetTimer };
}
