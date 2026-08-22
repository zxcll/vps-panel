-- 允许一条转发规则配多个入口节点。
--
-- 用途：把几台机器都设成入口，再把它们一起加进域名记录的候选列表。
-- 域名故障切换切到哪一台，转发都还通。
--
-- 原来的唯一索引是 (rule_id, position)，一个位置只能有一个节点。
-- 现在把节点也加进键里：入口（position 0）可以有多行，
-- 「同一位置不能重复同一个节点」这条约束仍然由数据库保证。
-- 「只有入口允许配多个」是业务规则，在 forwardplan.Expand 里校验。
--
-- DROP INDEX IF EXISTS 和 CREATE INDEX IF NOT EXISTS 都是幂等的，
-- 满足本项目「迁移每次启动全量重放」的要求。
DROP INDEX IF EXISTS idx_forward_hops_pos;

CREATE UNIQUE INDEX IF NOT EXISTS idx_forward_hops_pos_node
    ON forward_hops(rule_id, position, node_id);
