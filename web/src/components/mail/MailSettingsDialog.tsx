import { Button } from "@cloudflare/kumo/components/button";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogRoot,
  DialogTitle,
} from "@cloudflare/kumo/components/dialog";
import { Radio } from "@cloudflare/kumo/components/radio";
import { GearSix, X } from "@phosphor-icons/react";
import { useState } from "react";
import { AutoRefreshControl } from "@/components/mail/AutoRefreshControl";
import { DEFAULT_FOLDER_OPTIONS, type DefaultFolder } from "@/lib/mailPrefs";

interface MailSettingsDialogProps {
  defaultFolder: DefaultFolder;
  onDefaultFolderChange: (folder: DefaultFolder) => void;
  autoRunning: boolean;
  onAutoRefreshEnabled: () => void;
}

// 工具栏右侧原本并排摆着「默认打开分类」下拉和自动刷新开关,窄屏下这一整组
// 会被挤出可视区。改成一个齿轮入口,把这些设置收进弹窗,工具栏就只剩操作按钮。
export function MailSettingsDialog({
  defaultFolder,
  onDefaultFolderChange,
  autoRunning,
  onAutoRefreshEnabled,
}: MailSettingsDialogProps) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <Button
        variant="secondary"
        shape="square"
        size="sm"
        aria-label="信箱设置"
        title="信箱设置"
        onClick={() => setOpen(true)}
      >
        <GearSix size={16} />
      </Button>

      <DialogRoot open={open} onOpenChange={setOpen}>
        <Dialog size="base" className="p-0">
          <div className="flex items-center justify-between border-b border-kumo-line px-6 py-4">
            <DialogTitle className="text-lg font-semibold">信箱设置</DialogTitle>
            <DialogClose
              render={(props) => (
                <Button {...props} variant="secondary" shape="square" size="sm" aria-label="关闭">
                  <X size={18} />
                </Button>
              )}
            />
          </div>

          <div className="flex flex-col gap-6 p-6">
            <section className="flex flex-col gap-3">
              <div className="flex flex-col gap-1">
                <h3 className="text-sm font-medium text-kumo-default">信箱偏好</h3>
                <DialogDescription className="text-xs text-kumo-subtle">
                  下次打开邮箱时默认停在哪个分类。在邮件栏里切换分类只影响当前视图，不会改这里。
                </DialogDescription>
              </div>
              <Radio.Group
                aria-label="默认打开分类"
                appearance="card"
                orientation="horizontal"
                value={defaultFolder}
                onValueChange={(value: string) => onDefaultFolderChange(value as DefaultFolder)}
              >
                {DEFAULT_FOLDER_OPTIONS.map((option) => (
                  <Radio.Item
                    key={option.value}
                    value={option.value}
                    label={option.label}
                    description={option.description}
                  />
                ))}
              </Radio.Group>
            </section>

            <section className="flex flex-col gap-2 border-t border-kumo-line pt-5">
              <h3 className="text-sm font-medium text-kumo-default">自动刷新</h3>
              <AutoRefreshControl running={autoRunning} onEnabled={onAutoRefreshEnabled} />
            </section>
          </div>
        </Dialog>
      </DialogRoot>
    </>
  );
}
