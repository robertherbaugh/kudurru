-- Source ownership without inventing ownership of existing node-only cells.
-- NULL source means an opaque legacy contribution, retained unchanged.
-- Replacing the old conflict target also refuses already-connected old writers.
CREATE TABLE rollup_source_state (
 source integer PRIMARY KEY REFERENCES source(id),
 legacy_before bigint NOT NULL DEFAULT 0,
 facts_enabled boolean NOT NULL DEFAULT true,
 input_partial boolean NOT NULL DEFAULT false,
 hour_done bigint NOT NULL DEFAULT 0, day_done bigint NOT NULL DEFAULT 0,
 hour_after bigint NOT NULL DEFAULT 0, day_after bigint NOT NULL DEFAULT 0
);
CREATE TABLE rollup_input_hour (
 source integer NOT NULL REFERENCES source(id), ts bigint NOT NULL,
 revision bigint NOT NULL DEFAULT 0, head bigint NOT NULL DEFAULT 0, input_complete boolean NOT NULL DEFAULT true, minute_complete boolean NOT NULL DEFAULT true,
 PRIMARY KEY(source,ts)
);
CREATE TABLE rollup_bucket (
 source integer NOT NULL REFERENCES source(id), resolution bigint NOT NULL CHECK(resolution IN (3600,86400)),
 ts bigint NOT NULL, revision bigint NOT NULL DEFAULT 0, head bigint NOT NULL DEFAULT 0,
 retained boolean NOT NULL DEFAULT true,
 origin text NOT NULL CHECK(origin IN ('pending','synthetic','official')),
 error_key text NOT NULL DEFAULT '',
 PRIMARY KEY(source,resolution,ts)
);
CREATE TABLE rollup_repair_receipt (
 digest text PRIMARY KEY, source integer NOT NULL REFERENCES source(id),
 node_scope text NOT NULL, ts bigint NOT NULL, revision bigint NOT NULL,
 applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO kudurru_meta(key,value) VALUES ('rollup_backfill_after','{"after":0,"through":0}');
INSERT INTO kudurru_meta(key,value) SELECT 'rollup_legacy_before',(coalesce((SELECT max(boundary) FROM (SELECT max(ts)+60 AS boundary FROM rollup_1m UNION ALL SELECT max(ts)+60 AS boundary FROM rollup_client_1m UNION ALL SELECT max(ts)+60 AS boundary FROM rollup_qtype_1m UNION ALL SELECT max(ts)+60 AS boundary FROM rollup_upstream_1m UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_client_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_qtype_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_upstream_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_domain_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_list_1h UNION ALL SELECT max(ts)+3600 AS boundary FROM rollup_zone_1h UNION ALL SELECT max(ts)+86400 AS boundary FROM rollup_1d UNION ALL SELECT max(ts)+86400 AS boundary FROM rollup_client_1d UNION ALL SELECT max(ts)+86400 AS boundary FROM rollup_domain_1d) old),0))::text;
UPDATE kudurru_meta SET value=greatest(value::bigint,((extract(epoch FROM now())::bigint/86400)+1)*86400)::text WHERE key='rollup_legacy_before' AND value::bigint>0;
INSERT INTO rollup_source_state(source,legacy_before) SELECT id,(SELECT value::bigint FROM kudurru_meta WHERE key='rollup_legacy_before') FROM source WHERE kind='querylog';
ALTER TABLE rollup_1m ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_1m DROP CONSTRAINT rollup_1m_pkey;
ALTER TABLE rollup_1m ADD CONSTRAINT rollup_1m_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,source);
CREATE INDEX rollup_1m_source_ts ON rollup_1m(source,ts);
ALTER TABLE rollup_client_1m ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_client_1m DROP CONSTRAINT rollup_client_1m_pkey;
ALTER TABLE rollup_client_1m ADD CONSTRAINT rollup_client_1m_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,client,source);
CREATE INDEX rollup_client_1m_source_ts ON rollup_client_1m(source,ts);
ALTER TABLE rollup_qtype_1m ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_qtype_1m DROP CONSTRAINT rollup_qtype_1m_pkey;
ALTER TABLE rollup_qtype_1m ADD CONSTRAINT rollup_qtype_1m_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,qtype,source);
CREATE INDEX rollup_qtype_1m_source_ts ON rollup_qtype_1m(source,ts);
ALTER TABLE rollup_upstream_1m ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_upstream_1m DROP CONSTRAINT rollup_upstream_1m_pkey;
ALTER TABLE rollup_upstream_1m ADD CONSTRAINT rollup_upstream_1m_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,upstream,source);
CREATE INDEX rollup_upstream_1m_source_ts ON rollup_upstream_1m(source,ts);
ALTER TABLE rollup_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_1h DROP CONSTRAINT rollup_1h_pkey;
ALTER TABLE rollup_1h ADD CONSTRAINT rollup_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,source);
CREATE INDEX rollup_1h_source_ts ON rollup_1h(source,ts);
ALTER TABLE rollup_client_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_client_1h DROP CONSTRAINT rollup_client_1h_pkey;
ALTER TABLE rollup_client_1h ADD CONSTRAINT rollup_client_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,client,source);
CREATE INDEX rollup_client_1h_source_ts ON rollup_client_1h(source,ts);
ALTER TABLE rollup_qtype_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_qtype_1h DROP CONSTRAINT rollup_qtype_1h_pkey;
ALTER TABLE rollup_qtype_1h ADD CONSTRAINT rollup_qtype_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,qtype,source);
CREATE INDEX rollup_qtype_1h_source_ts ON rollup_qtype_1h(source,ts);
ALTER TABLE rollup_upstream_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_upstream_1h DROP CONSTRAINT rollup_upstream_1h_pkey;
ALTER TABLE rollup_upstream_1h ADD CONSTRAINT rollup_upstream_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,upstream,source);
CREATE INDEX rollup_upstream_1h_source_ts ON rollup_upstream_1h(source,ts);
ALTER TABLE rollup_domain_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_domain_1h DROP CONSTRAINT rollup_domain_1h_pkey;
ALTER TABLE rollup_domain_1h ADD CONSTRAINT rollup_domain_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,domain,source);
CREATE INDEX rollup_domain_1h_source_ts ON rollup_domain_1h(source,ts);
ALTER TABLE rollup_list_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_list_1h DROP CONSTRAINT rollup_list_1h_pkey;
ALTER TABLE rollup_list_1h ADD CONSTRAINT rollup_list_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,list,source);
CREATE INDEX rollup_list_1h_source_ts ON rollup_list_1h(source,ts);
ALTER TABLE rollup_zone_1h ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_zone_1h DROP CONSTRAINT rollup_zone_1h_pkey;
ALTER TABLE rollup_zone_1h ADD CONSTRAINT rollup_zone_1h_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,zone,source);
CREATE INDEX rollup_zone_1h_source_ts ON rollup_zone_1h(source,ts);
ALTER TABLE rollup_1d ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_1d DROP CONSTRAINT rollup_1d_pkey;
ALTER TABLE rollup_1d ADD CONSTRAINT rollup_1d_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,source);
CREATE INDEX rollup_1d_source_ts ON rollup_1d(source,ts);
ALTER TABLE rollup_client_1d ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_client_1d DROP CONSTRAINT rollup_client_1d_pkey;
ALTER TABLE rollup_client_1d ADD CONSTRAINT rollup_client_1d_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,client,source);
CREATE INDEX rollup_client_1d_source_ts ON rollup_client_1d(source,ts);
ALTER TABLE rollup_domain_1d ADD COLUMN source integer REFERENCES source(id);
ALTER TABLE rollup_domain_1d DROP CONSTRAINT rollup_domain_1d_pkey;
ALTER TABLE rollup_domain_1d ADD CONSTRAINT rollup_domain_1d_owner_unique UNIQUE NULLS NOT DISTINCT (ts,node,domain,source);
CREATE INDEX rollup_domain_1d_source_ts ON rollup_domain_1d(source,ts);

-- Retained facts carry exact source ownership, even for an old connected fact
-- shipper. The revision changes atomically with each committed input row.
CREATE FUNCTION track_rollup_input() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO rollup_source_state(source,legacy_before) VALUES(NEW.source,(SELECT value::bigint FROM kudurru_meta WHERE key='rollup_legacy_before')) ON CONFLICT(source) DO NOTHING;
 INSERT INTO rollup_input_hour(source,ts,revision,head)
 VALUES (NEW.source,(NEW.ts/1000000/3600)*3600,1,NEW.source_id)
 ON CONFLICT(source,ts) DO UPDATE SET revision=rollup_input_hour.revision+1,
 head=greatest(rollup_input_hour.head,excluded.head);
 UPDATE rollup_source_state SET hour_done=least(hour_done,(NEW.ts/1000000/3600)*3600), day_done=least(day_done,(NEW.ts/1000000/86400)*86400) WHERE source=NEW.source;
 INSERT INTO rollup_bucket(source,resolution,ts,origin,error_key)
 SELECT NEW.source,3600,(NEW.ts/1000000/3600)*3600,'pending','source_input_changed'
 WHERE EXISTS(SELECT 1 FROM ship_cursor WHERE source=NEW.source AND stream='source_rollup_1h' AND last_id>(NEW.ts/1000000/3600)*3600)
 ON CONFLICT(source,resolution,ts) DO NOTHING;
 INSERT INTO rollup_bucket(source,resolution,ts,origin,error_key)
 SELECT NEW.source,86400,(NEW.ts/1000000/86400)*86400,'pending','source_input_changed'
 WHERE EXISTS(SELECT 1 FROM ship_cursor WHERE source=NEW.source AND stream='source_rollup_1d' AND last_id>(NEW.ts/1000000/86400)*86400)
 ON CONFLICT(source,resolution,ts) DO NOTHING;
 UPDATE rollup_bucket SET origin='pending',error_key='source_input_changed' WHERE source=NEW.source AND NEW.source_id>head AND ((resolution=3600 AND ts=(NEW.ts/1000000/3600)*3600) OR (resolution=86400 AND ts=(NEW.ts/1000000/86400)*86400));
 RETURN NEW;
END $$;
CREATE TRIGGER query_rollup_input AFTER INSERT ON query FOR EACH ROW EXECUTE FUNCTION track_rollup_input();
INSERT INTO rollup_input_hour(source,ts,revision,head)
 SELECT source,(ts/1000000/3600)*3600,count(*),max(source_id) FROM query GROUP BY source,(ts/1000000/3600)*3600;
