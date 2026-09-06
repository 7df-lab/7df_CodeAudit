-- CodeAudit PostgreSQL 初始化（ADR-208 收敛：只建库，不建表）
-- 与生产口径一致（deploy/prod/deploy.sh init_dbs 只建空库）：
-- 表结构由各服务启动时自迁移；本文件的历史表 DDL 已落后真实 schema
-- （如缺 findings.verdict 列，曾致 result-service 启动失败），已删——见 git 历史。

CREATE DATABASE codeaudit_project;
CREATE DATABASE codeaudit_task;
CREATE DATABASE codeaudit_result;
