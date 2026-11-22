import { Button, RefreshButton } from "@cloudflare/kumo/components/button";
import { Checkbox } from "@cloudflare/kumo/components/checkbox";
import { CaretLeft, Envelope, X } from "@phosphor-icons/react";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  mailApi,
  type MailFolder,
  type Message,
  type MessageRef,
  type TenantRef,
  mailScopeKey,
} from "@/api/mail";
import { EmptyState } from "./EmptyState";
import { FolderTabs } from "./FolderTabs";
import { MessageBatchActions } from "./MessageBatchActions";
import { MessageFilterPills, type ReadFilter } from "./MessageFilterPills";
import { refKey, toRef } from "./messageRef";
import { MESSAGE_ROW_HEIGHT, MessageRow } from "./MessageRow";
import { VirtualList } from "./VirtualList";

// 一页 25 条。后端上限是 50（service.maxTop），但首屏越小出得越快，
// 而每一次拉取都要打上游，宁可让用户按需再要一页。
const PAGE_SIZE = 25;

// 列表缓存只解决「切回来先看到内容」:命中就先渲染上一份结果,同时照常请求。
// 服务端自己还有 60 秒缓存,所以这里的旧数据通常只落后一个请求,
// 而不是让用户对着「正在拉取」白屏。账号切换会卸载本组件,缓存必须放在模块级。
const LIST_CACHE_TTL = 5 * 60 * 1000;
const LIST_CACHE_MAX = 64;
interface CachedList {
  items: Message[];
  channel: string;
  reachedEnd: boolean;
  at: number;
}
const listCache = new Map<string, CachedList>();

function readListCache(key: string): CachedList | null {
  const hit = listCache.get(key);
  if (!hit) return null;
  if (Date.now() - hit.at > LIST_CACHE_TTL) {
    listCache.delete(key);
    return null;
  }
  return hit;
}

function writeListCache(key: string, value: CachedList) {
  if (!listCache.has(key) && listCache.size >= LIST_CACHE_MAX) {
    // 只缓存首页,丢最早写入的一条即可;不值得为「丢哪个」引入 LRU。
    const oldest = listCache.keys().next().value;
    if (oldest !== undefined) listCache.delete(oldest);
  }
  listCache.set(key, value);
}

// Page 是「某一批数据」的全部状态，key 标明它属于哪个账号/文件夹/第几次刷新。
// 收拢成一个对象是为了能在渲染期一次性重置——切账号或切文件夹时若用 effect 逐个
// setState，重置会晚一帧发生，那一帧里旧文件夹的邮件仍然挂在新文件夹的标签下，
// 用户此时点批量删除，删的是看不见的信。
interface Page {
  key: string;
  items: Message[];
  channel: string;
  reachedEnd: boolean;
  loading: boolean;
  error: string;
  selected: Set<string>;
}

const emptyPage = (key: string): Page => ({
  key,
  items: [],
  channel: "",
  reachedEnd: false,
  loading: true,
  error: "",
  selected: new Set(),
});

// pageFromCache 用缓存构造一个「有内容但仍在加载」的页。
// 直接用 emptyPage 会让切回看过的邮箱时又白屏一次,缓存的意义正是不白屏。
const pageFromCache = (key: string, listKey: string): Page | null => {
  const cached = readListCache(listKey);
  if (!cached) return null;
  return {
    key,
    items: cached.items,
    channel: cached.channel,
    reachedEnd: cached.reachedEnd,
    loading: true,
    error: "",
    selected: new Set(),
  };
};

const message = (e: unknown) => (e instanceof Error ? e.message : "加载失败");

const SKELETON_ROWS = [0, 1, 2, 3, 4, 5];

// 邮件类请求走上游,经常要一秒以上。有形状的占位比一句「正在拉取」更容易
// 让人判断页面还在动;行高与实际列表接近,数据到达时不会整块跳动。
function MessageListSkeleton() {
  return (
    <div
      className="flex-1 overflow-hidden px-3 py-2"
      role="status"
      aria-label="正在从服务器拉取邮件"
    >
      {SKELETON_ROWS.map((i) => (
        <div key={i} className="border-b border-kumo-line py-3">
          <div className="flex items-center gap-2">
            <div className="h-3 w-24 animate-pulse rounded bg-kumo-tint" />
            <div className="h-3 w-12 animate-pulse rounded bg-kumo-tint" />
          </div>
          <div className="mt-2 h-3 w-3/4 animate-pulse rounded bg-kumo-tint" />
        </div>
      ))}
      <p className="pt-3 text-sm text-kumo-subtle">正在从服务器拉取邮件...</p>
    </div>
  );
}

interface MessageListProps {
  tenantID: TenantRef;
  accountID: string;
  activeMessageKey: string | null;
  onOpen: (message: Message) => void;
  // 删除后要通知外层：正在看的那封如果被删了，详情栏必须一起关掉。
  onRemoved: (keys: string[]) => void;
  // 收起整栏收件箱。移动端是「返回账号列表」（那时账号列表被这一栏盖住了），
  // 桌面端是右上角那个关闭按钮——两边同一个动作，都回到「没有选中邮箱」。
  onClose: () => void;
  // 批量标已读后通知外层刷新账号列表里的未读标识。
  onAccountsChanged?: () => void;
  // 详情真正加载完成后由外层转交的置已读信号。seq 区分同一封邮件的多次打开。
  readSignal?: { accountID: string; message: Message; seq: number } | null;
}

export function MessageList({
  tenantID,
  accountID,
  activeMessageKey,
  onOpen,
  onRemoved,
  onClose,
  onAccountsChanged,
  readSignal,
}: MessageListProps) {
  const [folder, setFolder] = useState<MailFolder>("inbox");
  const [readFilter, setReadFilter] = useState<ReadFilter>("all");
  const [reloadToken, setReloadToken] = useState(0);
  // 用 mailScopeKey 而不是直接插 tenantID：scope 可能是对象，
  // 插进模板串会变成 "[object Object]"，于是所有租户共用同一个 key，
  // 管理员切到另一个租户时列表不重置——看到的还是上一个人的邮件。
  const key = `${mailScopeKey(tenantID)}|${accountID}|${folder}|${reloadToken}`;
  // 缓存键不含 reloadToken:手动刷新要沿用同一份旧数据,期间列表不清空。
  const listKey = `${mailScopeKey(tenantID)}|${accountID}|${folder}`;

  const [page, setPage] = useState(() => pageFromCache(key, listKey) ?? emptyPage(key));

  // 自动打开/切换邮箱走一分钟缓存;右上角手动刷新绕过缓存,但 5 秒内只能点一次。
  // 每一次强制刷新都是真实的上游调用并扣一次取件额度。
  const forceRef = useRef(false);
  const [refreshing, setRefreshing] = useState(false);
  const [cooldown, setCooldown] = useState(false);
  const cooldownTimer = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (cooldownTimer.current) window.clearTimeout(cooldownTimer.current);
    },
    [],
  );
  const manualRefresh = () => {
    if (cooldown || refreshing) return;
    forceRef.current = true;
    setRefreshing(true);
    setReloadToken((v) => v + 1);
    setCooldown(true);
    cooldownTimer.current = window.setTimeout(() => setCooldown(false), 5000);
  };

  // 渲染期同步重置（React 官方的 adjusting-state-when-props-change 模式）。
  // 这一版的输出会被 React 丢弃并立刻用新 state 重渲染，所以下面统一用 view 而不是 page。
  let view = page;
  if (page.key !== key) {
    // 仍然标记 loading:这是旧数据,后台请求还在跑,刷新按钮也不该被点。
    view = pageFromCache(key, listKey) ?? emptyPage(key);
    setPage(view);
  }

  // 所有写回都先比对 key：慢请求返回时用户可能已经切走了，
  // 这个判断让过期结果无害地落空（cleanup 的 ignore 只能挡住同一个 effect 实例）。
  const patch = useCallback(
    (k: string, fn: (prev: Page) => Page) => setPage((prev) => (prev.key === k ? fn(prev) : prev)),
    [],
  );

  useEffect(() => {
    let ignore = false;
    const force = forceRef.current;
    forceRef.current = false;
    void (async () => {
      try {
        const resp = await mailApi.messages(tenantID, accountID, {
          folder,
          top: PAGE_SIZE,
          refresh: force || undefined,
        });
        if (ignore) return;
        patch(key, (prev) => ({
          ...prev,
          items: resp.data.items,
          channel: resp.data.channel,
          reachedEnd: resp.data.items.length < PAGE_SIZE,
          loading: false,
        }));
      } catch (e) {
        if (!ignore) patch(key, (prev) => ({ ...prev, error: message(e), loading: false }));
      } finally {
        if (!ignore) {
          setRefreshing(false);
          // 服务端在每次取列表时都会写回账号的访问结果（成功或失败），
          // 手动刷新还会顺带更新未读数。不通知外层，账号行会一直停在
          // 打开这个邮箱之前的样子——令牌失效了还显示「有效」。
          onAccountsChanged?.();
        }
      }
    })();
    return () => {
      ignore = true;
    };
  }, [key, tenantID, accountID, folder, patch, onAccountsChanged]);

  // 每次拿到稳定结果就回写缓存。放在 effect 而不是 patch 内部:
  // 后者是在 setState 更新函数里做副作用,StrictMode 下会被调用两次。
  useEffect(() => {
    if (page.key !== key || page.loading || page.error) return;
    writeListCache(listKey, {
      items: page.items,
      channel: page.channel,
      reachedEnd: page.reachedEnd,
      at: Date.now(),
    });
  }, [page, key, listKey]);

  const loadMore = useCallback(
    async (skip: number) => {
      patch(key, (prev) => ({ ...prev, loading: true, error: "" }));
      try {
        const resp = await mailApi.messages(tenantID, accountID, { folder, skip, top: PAGE_SIZE });
        patch(key, (prev) => ({
          ...prev,
          items: [...prev.items, ...resp.data.items],
          reachedEnd: resp.data.items.length < PAGE_SIZE,
          loading: false,
        }));
      } catch (e) {
        patch(key, (prev) => ({ ...prev, error: message(e), loading: false }));
      }
    },
    [key, tenantID, accountID, folder, patch],
  );

  const toggle = useCallback(
    (m: Message) => {
      patch(key, (prev) => {
        const selected = new Set(prev.selected);
        if (!selected.delete(refKey(m))) selected.add(refKey(m));
        return { ...prev, selected };
      });
    },
    [key, patch],
  );

  // 批量操作后按返回结果就地改本地状态，而不是重新拉一次列表：
  // 重拉要再打一次上游、再扣一次配额，用户却只是标了个已读。
  const applyRead = useCallback(
    (done: MessageRef[]) => {
      const keys = new Set(done.map(refKey));
      // 默认保留选中:标已读后用户常常还要对同一批邮件做下一步(删除)。
      // 但「未读」筛选下这些行会立刻从列表里消失,留着选中等于让操作条
      // 对看不见的邮件生效,所以只在这种情况下把成功的那些移出选中。
      patch(key, (prev) => ({
        ...prev,
        items: prev.items.map((m) => (keys.has(refKey(m)) ? { ...m, is_read: true } : m)),
        selected:
          readFilter === "unread"
            ? new Set([...prev.selected].filter((k) => !keys.has(k)))
            : prev.selected,
      }));
      onAccountsChanged?.();
    },
    [key, patch, readFilter, onAccountsChanged],
  );

  // 详情真正加载出来后置为已读:列表状态与账号未读数一起更新,
  // 否则用户读完回来仍看到它标着未读。
  //
  // 触发条件由 MessageDetail 给出(服务端返回 is_read=false),不是点击时——
  // 详情取失败就不该把这封标成已读。走批量已读同一条接口,而不是把
  // 协议的 BODY.PEEK 改成非 PEEK:后者会让脚本拉详情也悄悄改状态,
  // 而且服务端不会扣账号的未读缓存。
  const markedSeqRef = useRef(0);
  useEffect(() => {
    if (!readSignal || readSignal.seq === markedSeqRef.current) return;
    // 切账号后本组件会重挂载,不能让上一个账号的信号落到新账号头上。
    if (readSignal.accountID !== accountID) return;
    markedSeqRef.current = readSignal.seq;
    // 列表里已经显示为已读就不再调接口:详情缓存里的 is_read 可能还是
    // 置已读之前的旧值,重复上报会多扣一次账号未读数。
    const current = view.items.find((m) => refKey(m) === refKey(readSignal.message));
    if (current?.is_read) return;
    void mailApi
      .markMessagesRead(tenantID, accountID, [{ ...toRef(readSignal.message), was_unread: true }])
      .then((resp) => {
        const done = resp.data.items.filter((i) => i.ok).map((i) => i.ref);
        if (done.length > 0) applyRead(done);
      })
      .catch(() => {
        // 置已读失败不影响阅读本身;保持未读,下次打开会重新上报。
      });
  }, [readSignal, tenantID, accountID, applyRead, view.items]);

  const applyDelete = useCallback(
    (done: MessageRef[]) => {
      const keys = new Set(done.map(refKey));
      patch(key, (prev) => ({
        ...prev,
        items: prev.items.filter((m) => !keys.has(refKey(m))),
        selected: new Set(),
      }));
      onRemoved([...keys]);
      onAccountsChanged?.();
    },
    [key, patch, onRemoved, onAccountsChanged],
  );

  const selectedMessages = view.items.filter((m) => view.selected.has(refKey(m)));

  // 已读筛选在前端做（上游列表接口没有这个参数），因此计数也按已加载的算。
  const counts = {
    all: view.items.length,
    unread: view.items.filter((m) => !m.is_read).length,
    read: view.items.filter((m) => m.is_read).length,
  };
  const visible =
    readFilter === "all"
      ? view.items
      : view.items.filter((m) => (readFilter === "unread" ? !m.is_read : m.is_read));

  // 全选只作用于当前筛选下可见的邮件:列表是按需分页加载的,
  // 把未加载的邮件也算进来,用户以为选了几封、实际会动到整个文件夹。
  const visibleKeys = visible.map(refKey);
  const selectedVisible = visibleKeys.filter((k) => view.selected.has(k)).length;
  const allVisibleSelected = visibleKeys.length > 0 && selectedVisible === visibleKeys.length;

  const toggleAllVisible = () =>
    patch(key, (prev) => {
      const selected = new Set(prev.selected);
      for (const k of visibleKeys) {
        if (allVisibleSelected) selected.delete(k);
        else selected.add(k);
      }
      return { ...prev, selected };
    });

  return (
    <section className="flex min-h-0 min-w-0 flex-1 flex-col">
      {/* 与详情 header 用同一套 items-start + px-4 py-3：两个面板顶部对齐、
          右边缘在各自激活状态下都贴着容器右边，两个 X 因此落在屏幕同一位置。
          关闭详情后列表面板展宽，列表 X 正好移到详情 X 原位，鼠标不用动即可连点。 */}
      <header className="flex shrink-0 items-start gap-2 overflow-x-auto border-b border-kumo-line px-4 py-3">
        <Button
          className="md:hidden"
          shape="square"
          size="sm"
          variant="ghost"
          icon={CaretLeft}
          aria-label="返回账号列表"
          onClick={onClose}
        />
        <FolderTabs value={folder} onChange={setFolder} disabled={view.loading} />
        <RefreshButton
          className="ml-auto"
          size="sm"
          variant="ghost"
          loading={view.loading || refreshing || cooldown}
          disabled={view.loading || refreshing || cooldown}
          aria-label={refreshing ? "正在刷新" : cooldown ? "刚刚刷新过" : "重新拉取"}
          onClick={manualRefresh}
        />
        {/* 桌面端的关闭。上面那个返回按钮是 md:hidden 的，宽屏下不渲染，
            这一栏得有自己的出口。 */}
        <Button
          className="hidden md:inline-flex"
          shape="square"
          size="sm"
          variant="ghost"
          icon={X}
          aria-label="关闭收件箱"
          onClick={onClose}
        />
      </header>

      <div className="flex h-(--ebx-subhead-h) shrink-0 items-center gap-2 overflow-x-auto border-b border-kumo-line px-3">
        <Checkbox
          checked={allVisibleSelected}
          indeterminate={selectedVisible > 0 && !allVisibleSelected}
          disabled={visibleKeys.length === 0}
          onCheckedChange={toggleAllVisible}
          aria-label="全选当前列表的邮件"
        />
        <MessageBatchActions
          tenantID={tenantID}
          accountID={accountID}
          messages={selectedMessages}
          onRead={applyRead}
          onDelete={applyDelete}
        />
        <MessageFilterPills value={readFilter} onChange={setReadFilter} counts={counts} />
      </div>

      {view.error && <p className="px-3 py-2 text-sm text-kumo-danger">{view.error}</p>}

      {view.loading && view.items.length === 0 ? (
        <MessageListSkeleton />
      ) : visible.length === 0 && !view.error ? (
        <EmptyState
          icon={Envelope}
          title={readFilter === "all" ? "这个文件夹里没有邮件" : "没有符合筛选条件的邮件"}
        />
      ) : (
        <VirtualList
          items={visible}
          rowHeight={MESSAGE_ROW_HEIGHT}
          className="min-h-0 flex-1"
          renderRow={(m) => (
            <MessageRow
              message={m}
              active={activeMessageKey === refKey(m)}
              checked={view.selected.has(refKey(m))}
              onOpen={onOpen}
              onToggle={toggle}
            />
          )}
        />
      )}

      {/* 用「加载更多」而不是滚动到底自动加载：每一页都是一次远端调用 +
          一次 daily_mail_fetch 扣减，随手滚两下就把当天配额刷掉说不过去。 */}
      {view.items.length > 0 && !view.reachedEnd && (
        <div className="border-t border-kumo-line p-2">
          <Button
            className="w-full"
            size="sm"
            variant="secondary"
            disabled={view.loading}
            onClick={() => void loadMore(view.items.length)}
          >
            {view.loading ? "加载中…" : "加载更多"}
          </Button>
        </div>
      )}

      {view.channel && (
        <p className="border-t border-kumo-line px-3 py-1 text-xs text-kumo-subtle">
          通道 {view.channel}
        </p>
      )}
    </section>
  );
}
