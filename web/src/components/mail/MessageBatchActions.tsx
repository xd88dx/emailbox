import { Button } from "@cloudflare/kumo/components/button";
import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { EnvelopeOpen, Trash } from "@phosphor-icons/react";
import { mailApi, type Message, type MessageRef, type TenantRef } from "@/api/mail";
import { useAsyncAction } from "@/lib/useAsyncAction";
import { toRef } from "./messageRef";

interface MessageBatchActionsProps {
  tenantID: TenantRef;
  accountID: string;
  messages: Message[];
  onRead: (done: MessageRef[]) => void;
  onDelete: (done: MessageRef[]) => void;
}

// 选中后出现在筛选行右侧的批量动作。原先它单独占一行, toolbar 与列表之间
// 会多出一条随选中数量出现/消失的空白;并进筛选行后,全选、计数、动作在同一处。
// 菜单里只放后端已有的能力(标记已读 / 删除),没有星标、归档这些本项目不支持的项。
export function MessageBatchActions({
  tenantID,
  accountID,
  messages,
  onRead,
  onDelete,
}: MessageBatchActionsProps) {
  const { error, pending, run } = useAsyncAction();
  if (messages.length === 0) return null;

  const refs = messages.map((message) => ({ ...toRef(message), was_unread: !message.is_read }));

  // 后端逐封返回成功与否(部分失败是常态:一封信可能已经在别处被删了),
  // 只把 ok 的那些应用到界面上,失败的留在原地并把条数说清楚。
  const act = (
    call: () => Promise<{ items: { ref: MessageRef; ok: boolean }[]; failed: number }>,
    apply: (done: MessageRef[]) => void,
  ) =>
    void run(async () => {
      const result = await call();
      apply(result.items.filter((i) => i.ok).map((i) => i.ref));
      if (result.failed > 0) throw new Error(`有 ${result.failed} 封操作失败`);
    });

  return (
    <>
      <span className="ml-auto shrink-0 text-xs text-kumo-subtle tabular-nums">
        已选中 {messages.length} 封
      </span>
      <DropdownMenu>
        <DropdownMenu.Trigger
          render={
            <Button variant="secondary" size="sm" disabled={pending}>
              批量操作
            </Button>
          }
        />
        <DropdownMenu.Content side="bottom" align="end" sideOffset={4} className="min-w-32">
          <DropdownMenu.Item
            icon={EnvelopeOpen}
            onClick={() =>
              act(
                () => mailApi.markMessagesRead(tenantID, accountID, refs).then((r) => r.data),
                onRead,
              )
            }
          >
            标记已读
          </DropdownMenu.Item>
          <DropdownMenu.Item
            icon={Trash}
            variant="danger"
            onClick={() =>
              act(
                () => mailApi.deleteMessages(tenantID, accountID, refs).then((r) => r.data),
                onDelete,
              )
            }
          >
            删除
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu>
      {error && <span className="shrink-0 text-xs text-kumo-danger">{error}</span>}
    </>
  );
}
