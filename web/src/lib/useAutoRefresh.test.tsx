import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/client";
import { useAutoRefresh } from "./useAutoRefresh";

const mocks = vi.hoisted(() => ({
  submitUnreadCheck: vi.fn(),
  get: vi.fn(),
}));

vi.mock("@/api/jobs", () => ({
  jobApi: {
    submitUnreadCheck: mocks.submitUnreadCheck,
    get: mocks.get,
  },
  isTerminal: (status: string) =>
    ["succeeded", "partial", "failed", "stopped", "interrupted"].includes(status),
}));

describe("useAutoRefresh", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mocks.submitUnreadCheck.mockReset();
    mocks.get.mockReset();
    mocks.submitUnreadCheck.mockResolvedValue({ data: { id: "job-1" } });
    mocks.get.mockResolvedValue({ data: { status: "succeeded" } });
  });
  afterEach(() => vi.useRealTimers());

  it("关闭时不提交任何任务", async () => {
    const onDone = vi.fn();
    renderHook(() => useAutoRefresh(false, 5, "t1", onDone));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60_000);
    });
    expect(mocks.submitUnreadCheck).not.toHaveBeenCalled();
  });

  it("开启后按间隔提交,任务结束再通知调用方重取列表", async () => {
    const onDone = vi.fn();
    const { result } = renderHook(() => useAutoRefresh(true, 5, "t1", onDone));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5 * 60_000 + 3_000);
    });

    expect(mocks.submitUnreadCheck).toHaveBeenCalledTimes(1);
    expect(mocks.get).toHaveBeenCalledWith("t1", "job-1");
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(result.current.running).toBe(false);
  });

  it("用户刚打开开关时立即检查,不等满一个间隔", async () => {
    const onDone = vi.fn();
    renderHook(() => useAutoRefresh(true, 5, "t1", onDone, 1));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_000);
    });
    expect(mocks.submitUnreadCheck).toHaveBeenCalledTimes(1);
  });

  it("撞上手动任务在跑(409)时静默跳过,且不卡在运行态", async () => {
    mocks.submitUnreadCheck.mockRejectedValue(new ApiError("已有任务", 409));
    const onDone = vi.fn();
    const { result } = renderHook(() => useAutoRefresh(true, 5, "t1", onDone));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5 * 60_000 + 1_000);
    });

    expect(onDone).not.toHaveBeenCalled();
    expect(result.current.running).toBe(false);
  });

  it("标签页在后台(hidden)时仍按间隔提交检查", async () => {
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => "hidden",
    });
    try {
      const onDone = vi.fn();
      renderHook(() => useAutoRefresh(true, 5, "t1", onDone));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(5 * 60_000 + 3_000);
      });

      expect(mocks.submitUnreadCheck).toHaveBeenCalledTimes(1);
      expect(onDone).toHaveBeenCalledTimes(1);
    } finally {
      delete (document as { visibilityState?: string }).visibilityState;
    }
  });

  // 中途关掉开关时旧的一轮还在轮询,收尾必须复位 running,
  // 否则工具栏的「刷新/已读」会一直因为 autoRunning 被禁用。
  it("运行途中关闭开关后不会卡在运行态", async () => {
    mocks.get.mockResolvedValue({ data: { status: "running" } });
    const onDone = vi.fn();
    const { result, rerender } = renderHook(
      ({ enabled }) => useAutoRefresh(enabled, 5, "t1", onDone, 1),
      { initialProps: { enabled: true } },
    );

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(result.current.running).toBe(true);

    rerender({ enabled: false });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_000);
    });

    expect(result.current.running).toBe(false);
    expect(onDone).not.toHaveBeenCalled();
  });
});
