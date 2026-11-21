-- 每日翻译次数配额。与 daily_mail_fetch 一样是「按天累加、走远端之前预扣」的频次类额度。
-- 翻译是显式付费动作,默认给一个保守值。管理员可在套餐页调整,-1 表示不限。
ALTER TABLE plans ADD COLUMN daily_translate INTEGER NOT NULL DEFAULT 200;