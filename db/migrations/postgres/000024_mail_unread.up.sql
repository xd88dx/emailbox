-- 未读计数是收信检查任务的元数据缓存,不是邮件本身:邮件仍然实时从上游拉取、不落库。
-- 它只服务于「列表置顶」与「这个邮箱有没有未读」两处展示。
ALTER TABLE mail_accounts ADD COLUMN unread_count INTEGER NOT NULL DEFAULT 0
    CHECK (unread_count >= 0);
ALTER TABLE mail_accounts ADD COLUMN unread_checked_at TIMESTAMPTZ;