-- The feed orders and pages by (coalesce(last_activity_at, 0), id) so the
-- cursor breaks ties (hub.md §4.7); index that expression.
DROP INDEX sessions_feed;
CREATE INDEX sessions_feed ON sessions(coalesce(last_activity_at, 0) DESC, id DESC) WHERE parent_session_id IS NULL;
