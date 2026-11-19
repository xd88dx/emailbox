import { Button } from "@cloudflare/kumo/components/button";
import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { Input } from "@cloudflare/kumo/components/input";
import { Select } from "@cloudflare/kumo/components/select";
import { CaretUpDown, GearSix, X } from "@phosphor-icons/react";
import { useState } from "react";
import type { AccountStatus, MailGroupNode, TenantRef } from "@/api/mail";
import { GroupManagerDialog } from "./GroupManagerDialog";
import { groupSelectItems } from "./groupOptions";

const STATUS_ITEMS = [
  { label: "全部状态", value: "" },
  { label: "正常", value: "active" },
  { label: "已停用", value: "disabled" },
  { label: "已封禁", value: "banned" },
];

interface AccountFilterBarProps {
  tenantID: TenantRef;
  groups: MailGroupNode[];
  groupID: string | null;
  onGroupChange: (groupID: string | null) => void;
  status: AccountStatus | "";
  onStatusChange: (status: AccountStatus | "") => void;
  // 关键字筛选走服务端的 q(匹配邮箱与备注),不是在本地过滤当前这一页:
  // 列表是分页的,本地过滤只会把「第 51 个之后的账号」变成搜不到。
  query: string;
  onQueryChange: (query: string) => void;
  onGroupsChanged: () => void;
}

// 这一栏负责**过滤当前账号列表**;在左侧 MailSidebar 隐藏的断点,分组管理入口也
// 跟着分组筛选放在这里。导入/导出/刷新和批量动作仍在 MailToolbar 里——一个动作只
// 出现在一个地方,用户才不用猜「这两个刷新按钮是不是不一样」。

export function AccountFilterBar({
  tenantID,
  groups,
  groupID,
  onGroupChange,
  status,
  onStatusChange,
  query,
  onQueryChange,
  onGroupsChanged,
}: AccountFilterBarProps) {
  const [managerOpen, setManagerOpen] = useState(false);
  const currentStatusLabel =
    STATUS_ITEMS.find((item) => item.value === status)?.label ?? "全部状态";
  return (
    <header className="flex shrink-0 flex-wrap items-center gap-3 border-b border-kumo-line px-4 py-3">
      {/* 分组下拉只在窄屏出现:≥1280 时左侧有完整的分组列表,这里再放一个是重复的。
          768~1280 收起左栏是 06 文档定的响应式方案,但分组切换不能跟着一起消失。 */}
      <div className="flex min-w-0 items-center gap-2 xl:hidden">
        <Select
          className="w-40"
          size="sm"
          aria-label="按分组筛选"
          items={groupSelectItems(groups, { allLabel: "全部分组", counts: true })}
          value={groupID ?? ""}
          onValueChange={(value: string | null) => onGroupChange(value || null)}
        />
        <Button
          size="sm"
          variant="secondary"
          icon={GearSix}
          aria-label="管理分组"
          title="管理分组"
          onClick={() => setManagerOpen(true)}
        >
          管理
        </Button>
      </div>

      {/* 输入框 + 状态下拉的总宽度与工具栏的「已读/启用/停用/删除」组一致,
          右边缘落在同一条竖线上;
          右侧滑出的邮件面板也以那条线为左边界(--ebx-action-row-w)。 */}
      <div className="flex min-w-0 flex-1 items-center gap-3 xl:w-(--ebx-action-row-w) xl:flex-none">
        {/* 清楚按钮只在有内容时出现——常驻的空按钮占着一个点击位却什么也不做。 */}
        <div className="relative min-w-0 flex-1">
          <Input
            size="sm"
            className="w-full pr-7"
            placeholder="筛选邮箱或备注"
            aria-label="按关键字筛选"
            value={query}
            onChange={(e) => onQueryChange(e.target.value)}
          />
          {query && (
            <button
              type="button"
              aria-label="清除关键字"
              className="absolute inset-y-0 right-0 flex w-7 items-center justify-center text-kumo-subtle hover:text-kumo-strong"
              onClick={() => onQueryChange("")}
            >
              <X size={12} />
            </button>
          )}
        </div>
        {/* 用 DropdownMenu 而不是 Select:Kumo 的 Select 内置 Positioner 会去对齐
            选中项,选项少、下方空间紧时可能向上翻。这里显式 side="bottom"。
            筛选的取值集合很小,不需要 Select 的搜索/多选能力。 */}
        <DropdownMenu>
          <DropdownMenu.Trigger
            render={
              <Button
                variant="secondary"
                size="sm"
                className="w-32 shrink-0"
                aria-label="按状态筛选"
              >
                <span className="flex w-full items-center justify-between gap-2">
                  <span className="truncate">{currentStatusLabel}</span>
                  <CaretUpDown size={12} className="shrink-0 text-kumo-subtle" />
                </span>
              </Button>
            }
          />
          <DropdownMenu.Content side="bottom" align="start" sideOffset={4} className="min-w-32">
            {STATUS_ITEMS.map((item) => (
              <DropdownMenu.Item
                key={item.value}
                selected={status === item.value}
                onClick={() => onStatusChange(item.value as AccountStatus | "")}
              >
                {item.label}
              </DropdownMenu.Item>
            ))}
          </DropdownMenu.Content>
        </DropdownMenu>
      </div>

      {managerOpen && (
        <GroupManagerDialog
          tenantID={tenantID}
          groups={groups}
          onClose={() => setManagerOpen(false)}
          onGroupsChanged={onGroupsChanged}
        />
      )}
    </header>
  );
}
