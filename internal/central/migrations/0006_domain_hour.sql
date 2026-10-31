-- Exact retained-fact index, separate from F71 completion/provenance state.
-- Drain existing writers in source-then-query order and exclude inserts until
-- both seeding and trigger installation commit. Otherwise an already-connected
-- writer could commit after the seed snapshot but before the trigger exists.
LOCK TABLE source IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE query IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE query_domain_hour (
 ts bigint NOT NULL, node integer NOT NULL REFERENCES dict_node(id),
 source integer NOT NULL REFERENCES source(id),
 domain bigint NOT NULL REFERENCES dict_domain(id), total bigint NOT NULL,
 blocked bigint NOT NULL, first_ts bigint NOT NULL, last_ts bigint NOT NULL,
 PRIMARY KEY(ts,node,source,domain)
);
-- An always-empty FK guard blocks already-connected pre-v6 janitors/purgers
-- from dropping/truncating a fact partition without maintaining the index.
-- PostgreSQL automatically extends the dependency to future partitions.
-- New purge includes this empty guard; new retention detaches before DROP.
CREATE TABLE query_domain_guard (
 id bigint, ts bigint, CHECK(false),
 FOREIGN KEY(id,ts) REFERENCES query(id,ts)
);
CREATE INDEX query_domain_hour_node ON query_domain_hour(node);
CREATE INDEX query_domain_hour_domain ON query_domain_hour(domain);
INSERT INTO query_domain_hour
 SELECT (ts/3600000000)*3600,node,source,domain,count(*),sum((status BETWEEN 16 AND 23)::integer),min(ts),max(ts)
 FROM query GROUP BY 1,2,3,4;
CREATE FUNCTION track_domain_hour() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  INSERT INTO query_domain_hour VALUES((NEW.ts/3600000000)*3600,NEW.node,NEW.source,NEW.domain,1,(NEW.status BETWEEN 16 AND 23)::integer,NEW.ts,NEW.ts)
  ON CONFLICT(ts,node,source,domain) DO UPDATE SET total=query_domain_hour.total+1,
   blocked=query_domain_hour.blocked+excluded.blocked,
   first_ts=least(query_domain_hour.first_ts,excluded.first_ts),last_ts=greatest(query_domain_hour.last_ts,excluded.last_ts);
  RETURN NEW;
 END IF;
 UPDATE query_domain_hour SET total=total-1,blocked=blocked-(OLD.status BETWEEN 16 AND 23)::integer
 WHERE ts=(OLD.ts/3600000000)*3600 AND node=OLD.node AND source=OLD.source AND domain=OLD.domain;
 DELETE FROM query_domain_hour WHERE ts=(OLD.ts/3600000000)*3600 AND node=OLD.node AND source=OLD.source AND domain=OLD.domain AND total=0;
 RETURN OLD;
END $$;
-- Run after query_rollup_input: F71 input state precedes the domain index,
-- matching purge/retention lock order. PostgreSQL orders triggers by name.
CREATE TRIGGER query_z_domain_insert AFTER INSERT ON query FOR EACH ROW EXECUTE FUNCTION track_domain_hour();
CREATE TRIGGER query_domain_delete AFTER DELETE ON query FOR EACH ROW EXECUTE FUNCTION track_domain_hour();
