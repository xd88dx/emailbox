import { beforeEach, describe, expect, it } from "vitest";
import { useJobStore } from "./jobStore";

const item = (accountID: string, unread: number) => ({
  accountID,
  email: `${accountID}@example.com`,
  status: "success" as const,
  errorKind: "",
  error: "",
  unread,
});

describe("jobStore.clearUnread", () => {
  beforeEach(() => {
    useJobStore.setState({
      jobType: "unread_check",
      byAccount: { a: item("a", 4), b: item("b", 1) },
    });
  });

  // 收信检查运行期间徽标优先读快照。本地标已读后快照已过期,
  // 必须丢掉落回库值,但不能把检查结论(绿/红点)一起丢掉。
  it("只清目标账号的未读数,保留检查结论", () => {
    useJobStore.getState().clearUnread("a");
    const account = useJobStore.getState().byAccount.a;
    expect(account.unread).toBeUndefined();
    expect(account.status).toBe("success");
    expect(useJobStore.getState().byAccount.b.unread).toBe(1);
  });

  it("没有快照时保持不变", () => {
    const before = useJobStore.getState().byAccount;
    useJobStore.getState().clearUnread("missing");
    expect(useJobStore.getState().byAccount).toBe(before);
  });
});
