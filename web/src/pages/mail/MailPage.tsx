import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { isTerminal, jobApi, type RefreshStats } from "@/api/jobs";
import {
  mailApi,
  mailScopeKey,
  type AccountStatus,
  type MailAccount,
  type MailGroupNode,
  type MailScope,
  type Message,
  type RefreshStatus,
  type TenantRef,
} from "@/api/mail";
import { AccountDrawer } from "@/components/mail/AccountDrawer";
import { AccountFilterBar } from "@/components/mail/AccountFilterBar";
import { AccountList } from "@/components/mail/AccountList";
import { ExportDialog } from "@/components/mail/ExportDialog";
import { MailShell } from "@/components/mail/MailShell";
import { MailSidebar } from "@/components/mail/MailSidebar";
import { MailStatusBar } from "@/components/mail/MailStatusBar";
import { MailToolbar } from "@/components/mail/MailToolbar";
import { MessageDetail } from "@/components/mail/MessageDetail";
import { MessageList } from "@/components/mail/MessageList";
import { refKey } from "@/components/mail/messageRef";
import { useAutoRefresh } from "@/lib/useAutoRefresh";
import { useAuthStore } from "@/store/authStore";
import { useJobStore } from "@/store/jobStore";
import { useSelectionStore } from "@/store/selectionStore";
import { useTenantStore } from "@/store/tenantStore";

interface MailPageProps {
  // scope 由管理员视图传入（/admin/tenants/:id/mail）。不传时看的是自己的工作空间。
  // 整棵组件树对这两种情形无感知——接口是同构的，差别只有请求前缀。
  scope?: MailScope;
}

export default function MailPage({ scope }: MailPageProps = {}) {
  const activeTenantID = useTenantStore((s) => s.activeTenant?.id) ?? "";

  // scope 是调用方每次渲染新建的对象字面量，直接用会让下面那个取数 effect
  // 每渲染一次就重拉一次。按内容记忆化之后，引用只在真正换租户时才变。
  const scopeTenantID = scope?.tenantID;
  const scopeAdmin = scope?.admin;
  const tenantID = useMemo<TenantRef>(
    () => (scopeTenantID ? { tenantID: scopeTenantID, admin: scopeAdmin } : activeTenantID),
    [scopeTenantID, scopeAdmin, activeTenantID],
  );
  const tenantKey = mailScopeKey(tenantID);
  const [groups, setGroups] = useState<MailGroupNode[]>([]);
  const [accounts, setAccounts] = useState<MailAccount[]>([]);
  const [total, setTotal] = useState(0);
  const [groupID, setGroupID] = useState<string | null>(null);
  // 搜索词来自 URL（筛选栏的关键字输入框写进去的），不在这里另存一份 state：
  // 两个真源迟早会分叉，表现为「地址栏写着 ?q=abc，列表却是全量」。
  const [searchParams, setSearchParams] = useSearchParams();
  const query = searchParams.get("q") ?? "";
  const [status, setStatus] = useState<AccountStatus | "">("");
  // 左栏那一段筛的是 refresh_status（登录成功/失败/从未），
  // 和中栏下拉的 status（正常/停用/封禁）是两个维度，可以叠加。
  const [refreshStatus, setRefreshStatus] = useState<RefreshStatus | "">("");
  // stats 在这里取一次，左栏的计数和底部状态条共用，不各拉一遍。
  const [stats, setStats] = useState<RefreshStats | null>(null);
  // 分页。后端每页默认 50、上限 200（model.DefaultAccountPageSize），
  // 而此前前端既不传 page 也没有翻页控件——账号超过 50 个时，
  // 第 51 个往后的账号在界面上根本不存在，顶部却还写着「共 N 个账号」。
  const [page, setPage] = useState(1);
  const [perPage, setPerPage] = useState(50);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const clearSelection = useSelectionStore((s) => s.clear);
  // 选择器必须返回稳定引用：zustand v5 走 useSyncExternalStore，
  // 每次调用都新建数组会让 React 认为快照一直在变，直接死循环（React error #185）。
  // 取回 Set 再 useMemo 成数组。
  const selectedSet = useSelectionStore((s) => s.selected);
  const selectedIDs = useMemo(() => Array.from(selectedSet), [selectedSet]);
  // 收信检查任务的逐账号结果,用来在列表里实时显示绿/红点。
  const jobType = useJobStore((s) => s.jobType);
  const checkResults = useJobStore((s) => s.byAccount);
  const jobStatus = useJobStore((s) => s.status);
  const clearUnread = useJobStore((s) => s.clearUnread);
  // 检查结果只在任务运行期间当实时值用。任务结束后库里已经有同一次检查
  // 写回的数字，继续拿快照覆盖会让「已读/删除后未读数减少」看不见。
  // 绿/红点仍然按快照显示，它们表达的是定性结论，不受这个切换影响。
  const checkLive = jobType === "unread_check" && !(jobStatus && isTerminal(jobStatus));

  // 右边两栏各自跟着一个选择：看哪个邮箱的信、看哪封信。
  // editAccount 是另一条线（改配置），和「看信」互不影响，所以分开存。
  const [activeAccount, setActiveAccount] = useState<MailAccount | null>(null);
  const [openMessage, setOpenMessage] = useState<Message | null>(null);
  // 详情真正加载完成后由 MessageDetail 通知,列表层据此置已读。
  // seq 让同一封邮件每次重新打开都能上报一次(上一次可能没置成功)。
  const [readSignal, setReadSignal] = useState<{
    accountID: string;
    message: Message;
    seq: number;
  } | null>(null);
  const [editAccount, setEditAccount] = useState<MailAccount | null>(null);
  const [exporting, setExporting] = useState(false);

  // reloadToken 让「刷新」按钮和批量操作完成后能重新触发下面的 effect，
  // 而不需要在事件处理器里再写一份取数逻辑。
  const [reloadToken, setReloadToken] = useState(0);
  // 邮件返回后只需要重取账号列表(令牌/健康状态可能有变化),
  // 分组与统计跟打开邮箱无关。共用一个 token 会让一次打开追加三个请求。
  const [accountsToken, setAccountsToken] = useState(0);
  const reload = useCallback(() => setReloadToken((v) => v + 1), []);
  const reloadAccounts = useCallback(() => setAccountsToken((v) => v + 1), []);
  const activeAccountID = activeAccount?.id;
  // 邮件列表里标已读/删除后:先让该账号的实时未读快照失效,再重取账号列表。
  // 收信检查任务运行期间快照可能早于这次操作,不失效会把刚减少的未读数盖回去。
  const onMessagesChanged = useCallback(() => {
    if (activeAccountID) clearUnread(activeAccountID);
    reloadAccounts();
  }, [activeAccountID, clearUnread, reloadAccounts]);

  // 自动收信检查:用户没显式设置时跟随部署默认(null 不是 false)。
  // 关闭状态也保留 minutes,重新打开时沿用上次选的间隔。
  const autoRefreshDefault = useAuthStore((s) => s.autoRefreshDefaultMinutes);
  const autoRefreshEnabled = useAuthStore((s) => s.user?.auto_refresh_enabled ?? null);
  const autoRefreshMinutes = useAuthStore((s) => s.user?.auto_refresh_minutes ?? null);
  const autoEnabled = autoRefreshEnabled ?? autoRefreshDefault > 0;
  const autoMinutes = autoRefreshMinutes ?? (autoRefreshDefault > 0 ? autoRefreshDefault : 10);
  // 用户刚从关闭切到开启时递增,让 hook 立即跑一轮而不是等满间隔。
  const [autoKick, setAutoKick] = useState(0);
  const autoRefresh = useAutoRefresh(autoEnabled, autoMinutes, tenantID, reloadAccounts, autoKick);

  // 分组与统计只在换租户或任务结束/批量操作后重取:它们与筛选条件无关。
  // 之前它们和账号列表串在同一个 Promise.all 里,每切换一次状态都要等
  // 三个请求中最慢的那个,下拉看起来就像卡住了。
  useEffect(() => {
    if (!tenantID) return undefined;
    let ignore = false;
    void (async () => {
      try {
        const [groupResp, statsResp] = await Promise.all([
          mailApi.groups(tenantID),
          // 统计失败不该让整页报错:左栏计数少几个数字,
          // 远不如「账号列表明明拿到了却显示加载失败」严重。
          jobApi.stats(tenantID).catch(() => null),
        ]);
        if (ignore) return;
        setGroups(groupResp.data);
        if (statsResp) setStats(statsResp.data);
      } catch (e) {
        if (!ignore) setError(e instanceof Error ? e.message : "加载失败");
      }
    })();
    return () => {
      ignore = true;
    };
  }, [tenantID, reloadToken]);

  useEffect(() => {
    if (!tenantID) return undefined;
    // ignore 防止「先发的慢请求后到」把新筛选条件的结果覆盖掉——
    // 用户快速切换分组时这非常容易发生。
    let ignore = false;
    void (async () => {
      try {
        const accountResp = await mailApi.accounts(tenantID, {
          group_id: groupID ?? undefined,
          q: query || undefined,
          status: status || undefined,
          refresh_status: refreshStatus || undefined,
          page,
          limit: perPage,
        });
        if (ignore) return;
        const items = accountResp.data.items;
        setAccounts(items);
        setTotal(accountResp.data.pagination.total);
        // 正在看信的那个账号如果被新的筛选条件排除掉了（或者干脆被删了），
        // 右边两栏必须跟着收起来，否则界面上会出现一个左边根本找不到的邮箱。
        setActiveAccount((prev) => (prev && items.some((a) => a.id === prev.id) ? prev : null));
        setError("");
      } catch (e) {
        if (ignore) return;
        setError(e instanceof Error ? e.message : "加载失败");
      } finally {
        if (!ignore) setLoading(false);
      }
    })();
    return () => {
      ignore = true;
    };
  }, [tenantID, groupID, query, status, refreshStatus, page, perPage, reloadToken, accountsToken]);

  // 换筛选条件要做两件事：清空选中集、回到第 1 页。
  //
  // - 留着上一批选中项，用户在新视图里点「批量删除」会删掉屏幕上根本看不见的账号。
  // - 换了条件还停在第 7 页，看到的多半是一片空白。
  //
  // 放在事件处理器里而不是 effect：effect 里同步 setState 会触发级联渲染
  // （eslint 的 react-hooks/set-state-in-effect 就是拦这个），而且要晚一帧——
  // 那一帧里旧 page 配着新条件，会先白发一次请求再被下一次覆盖。
  const resetPaging = useCallback(() => {
    setPage(1);
    clearSelection();
  }, [clearSelection]);

  const changeGroup = useCallback(
    (value: string | null) => {
      setGroupID(value);
      resetPaging();
    },
    [resetPaging],
  );
  const changeStatus = useCallback(
    (value: AccountStatus | "") => {
      setStatus(value);
      resetPaging();
    },
    [resetPaging],
  );
  const changeRefreshStatus = useCallback(
    (value: RefreshStatus | "") => {
      setRefreshStatus(value);
      resetPaging();
    },
    [resetPaging],
  );
  const changeQuery = useCallback(
    (value: string) => {
      // replace: 逐字输入不该在浏览器历史里留下几十条记录。
      // 清除按钮也只是把 q 从 URL 里删掉,列表随 effect 重新取数。
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (value) next.set("q", value);
          else next.delete("q");
          return next;
        },
        { replace: true },
      );
      resetPaging();
    },
    [setSearchParams, resetPaging],
  );

  const closeAccount = useCallback(() => {
    setActiveAccount(null);
    setOpenMessage(null);
    setReadSignal(null);
  }, []);

  // 点已经打开的那个邮箱＝收起右侧收件箱。桌面端不给这条路的话，
  // 收件箱一旦打开就关不掉了——那一栏的返回按钮只在移动端可见。
  const selectAccount = useCallback((account: MailAccount) => {
    setActiveAccount((prev) => (prev?.id === account.id ? null : account));
    setOpenMessage(null); // 换邮箱等于换一批信,上一封的详情留着没有意义
    setReadSignal(null); // 不能让上一个账号的置已读信号落到新账号上
  }, []);

  // 详情数据真正到手、且服务端说仍未读时才会被调用。
  const onDetailLoaded = useCallback(
    (message: Message) => {
      setReadSignal((prev) => ({
        accountID: activeAccount?.id ?? "",
        message,
        seq: (prev?.seq ?? 0) + 1,
      }));
    },
    [activeAccount?.id],
  );

  // 批量删除掉的信如果正好是详情栏里那封，详情要一起关掉——
  // 否则用户还能在那儿点附件下载，请求打到一封已经不存在的邮件上。
  const onMessagesRemoved = useCallback((keys: string[]) => {
    setOpenMessage((prev) => (prev && keys.includes(refKey(prev)) ? null : prev));
  }, []);

  if (!tenantKey) {
    return <p className="p-8 text-sm text-kumo-subtle">正在加载…</p>;
  }

  // 移动端一次只显示一栏，靠层级往下走：账号 → 邮件 → 详情。
  // 返回的路径分别是邮件栏的返回按钮和详情栏的关闭按钮。
  const pane = openMessage && activeAccount ? "detail" : activeAccount ? "messages" : "accounts";
  const paneClass = (self: string) => `${pane === self ? "flex" : "hidden"} md:flex`;
  const messageWidth = openMessage ? "md:w-80 md:flex-none xl:w-96" : "flex-1";
  // 1280~1535px 时正文需要优先拿到空间，账号窄栏暂时折进层级导航；
  // 超宽屏再把它留在最左侧，方便在读信时切换账号。
  const messagesPane = activeAccount ? (
    <div
      className={`${paneClass("messages")} min-h-0 min-w-0 flex-col ${messageWidth} transition-[width] duration-200`}
    >
      <MessageList
        tenantID={tenantID}
        accountID={activeAccount.id}
        activeMessageKey={openMessage ? refKey(openMessage) : null}
        onOpen={setOpenMessage}
        onRemoved={onMessagesRemoved}
        onClose={closeAccount}
        onAccountsChanged={onMessagesChanged}
        readSignal={readSignal}
      />
    </div>
  ) : null;
  const detailPane =
    activeAccount && openMessage ? (
      <div
        className={`${paneClass("detail")} min-h-0 min-w-0 flex-1 flex-col border-l border-kumo-line`}
      >
        <MessageDetail
          tenantID={tenantID}
          accountID={activeAccount.id}
          message={openMessage}
          onClose={() => setOpenMessage(null)}
          onLoaded={onDetailLoaded}
        />
      </div>
    ) : null;

  const accountsPane = (
    <section className="relative flex min-h-0 min-w-0 flex-1 flex-col">
      <AccountFilterBar
        tenantID={tenantID}
        groups={groups}
        groupID={groupID}
        onGroupChange={changeGroup}
        status={status}
        onStatusChange={changeStatus}
        query={query}
        onQueryChange={changeQuery}
        onGroupsChanged={reload}
      />

      {error && <p className="px-4 py-3 text-sm text-kumo-danger">{error}</p>}

      <AccountList
        accounts={accounts}
        loading={loading}
        activeID={activeAccount?.id ?? null}
        onSelect={selectAccount}
        onEdit={setEditAccount}
        page={page}
        perPage={perPage}
        total={total}
        onPageChange={setPage}
        onPerPageChange={setPerPage}
        checkResults={jobType === "unread_check" ? checkResults : undefined}
        unreadFromCheck={checkLive}
      />

      {/* 邮箱面板从右侧滑出,不参与主布局。它挂在账号列表这一栏里,
          所以收起左侧栏时面板会跟着列表一起左移;左边界等于「筛选栏右边缘 + 16px」,
          与筛选输入框距左边线的间隔一致。移动端铺满整栏。 */}
      <div
        className={`absolute inset-y-0 right-0 z-20 flex w-full flex-col border-l border-kumo-line bg-kumo-base transition-transform duration-200 md:w-[min(100%,48rem)] xl:left-[calc(2rem+var(--ebx-action-row-w))] xl:w-auto ${
          activeAccount ? "translate-x-0" : "translate-x-full"
        }`}
        aria-hidden={!activeAccount}
      >
        {activeAccount && (
          <div key={activeAccount.id} className="ebx-panel-in flex min-h-0 min-w-0 flex-1">
            {messagesPane}
            {detailPane}
          </div>
        )}
      </div>
    </section>
  );

  return (
    <MailShell
      toolbar={
        <MailToolbar
          tenantID={tenantID}
          onReload={reload}
          onExport={() => setExporting(true)}
          autoRunning={autoRefresh.running}
          onManualCheckStarted={autoRefresh.resetTimer}
          onAutoRefreshEnabled={() => setAutoKick((v) => v + 1)}
        />
      }
      status={<MailStatusBar stats={stats} />}
    >
      {/* 左栏只在 ≥1280 时并列显示，再窄就折进筛选栏的下拉里（06 文档 §5.1）。 */}
      <MailSidebar
        tenantID={tenantID}
        groups={groups}
        groupID={groupID}
        onGroupChange={changeGroup}
        refreshStatus={refreshStatus}
        onRefreshStatusChange={changeRefreshStatus}
        stats={stats}
        onGroupsChanged={reload}
      />

      {accountsPane}

      {exporting && (
        <ExportDialog
          tenantID={tenantID}
          groupID={groupID}
          selectedIDs={selectedIDs}
          onClose={() => setExporting(false)}
        />
      )}

      {editAccount && (
        <AccountDrawer
          tenantID={tenantID}
          account={editAccount}
          groups={groups}
          onClose={() => setEditAccount(null)}
          onSaved={reload}
        />
      )}
    </MailShell>
  );
}
