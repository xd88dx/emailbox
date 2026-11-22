-- 用户级「收信检查自动刷新」偏好。NULL 表示跟随部署默认(UI_AUTO_REFRESH_MINUTES),
-- 与 0/false 区分:用户显式关闭后,管理员改默认值不该把他重新打开。
ALTER TABLE users ADD COLUMN auto_refresh_enabled INTEGER
    CHECK (auto_refresh_enabled IN (0, 1));
ALTER TABLE users ADD COLUMN auto_refresh_minutes INTEGER
    CHECK (auto_refresh_minutes IS NULL OR
           (auto_refresh_minutes >= 5 AND auto_refresh_minutes <= 1440));