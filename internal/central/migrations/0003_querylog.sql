-- 0003: the query log and statistics of every node (D160).
--
-- The tables mirror querylog.db (internal/querylog/sqlitelog, §11.2) with
-- central dictionary ids, so one reader answers both: query.ts is unix µs,
-- rollup ts the unix second of the UTC bucket, and every rollup row keeps
-- its node, so a cluster total is a sum over nodes. Nodes ship their rows
-- by (source, local id) with the cursor in the same transaction.

CREATE TABLE dict_node     (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE dict_domain   (id bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL UNIQUE, etld1 text, first_seen bigint, last_seen bigint);
CREATE TABLE dict_client   (id bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY, ip text NOT NULL, name text, grp text, first_seen bigint, last_seen bigint,
	UNIQUE NULLS NOT DISTINCT (ip, name));
CREATE TABLE dict_upstream (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, addr text NOT NULL UNIQUE);
CREATE TABLE dict_zone     (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE dict_list     (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE dict_policy   (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL UNIQUE);

-- Facts, one partition per UTC day (created ahead by the leader and on
-- demand by shippers; expired partitions are dropped whole). id is the
-- central row id: the query log pages on it, newest first.
CREATE SEQUENCE query_id_seq;
CREATE TABLE query (
	id          bigint   NOT NULL DEFAULT nextval('query_id_seq'),
	ts          bigint   NOT NULL,
	latency_us  bigint,
	node        integer  NOT NULL,
	source      integer  NOT NULL,
	source_id   bigint   NOT NULL,
	proto       smallint NOT NULL,
	client      bigint   NOT NULL,
	domain      bigint   NOT NULL,
	qtype       integer  NOT NULL,
	qclass      integer  NOT NULL DEFAULT 1,
	status      smallint NOT NULL,
	rcode       smallint NOT NULL,
	reply_type  smallint NOT NULL,
	upstream    integer,
	zone        integer,
	list        integer,
	rule        text,
	cname       bigint,
	dnssec      smallint NOT NULL DEFAULT 0,
	ede         integer,
	flags       integer  NOT NULL DEFAULT 0,
	ecs         text,
	trace_id    bytea,
	answer      text,
	policy      integer,
	PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);
ALTER SEQUENCE query_id_seq OWNED BY query.id;
CREATE INDEX query_ts        ON query (ts);
CREATE INDEX query_client_ts ON query (client, ts);
CREATE INDEX query_domain_ts ON query (domain, ts);
CREATE INDEX query_status_ts ON query (status, ts) WHERE status >= 16;

CREATE TABLE rollup_1m (
	ts bigint NOT NULL, node integer NOT NULL,
	total bigint NOT NULL, blocked bigint NOT NULL, cached bigint NOT NULL, forwarded bigint NOT NULL, authoritative bigint NOT NULL, dropped bigint NOT NULL,
	rc_noerror bigint, rc_nxdomain bigint, rc_servfail bigint, rc_refused bigint, rc_other bigint,
	udp bigint, tcp bigint, dot bigint, doh bigint, doq bigint,
	lat_sum_us bigint, lat_cnt bigint,
	lat_b0 bigint, lat_b1 bigint, lat_b2 bigint, lat_b3 bigint, lat_b4 bigint, lat_b5 bigint, lat_b6 bigint, lat_b7 bigint, lat_b8 bigint, lat_b9 bigint, lat_b10 bigint, lat_b11 bigint,
	PRIMARY KEY (ts, node)
);
CREATE TABLE rollup_1h (
	ts bigint NOT NULL, node integer NOT NULL,
	total bigint NOT NULL, blocked bigint NOT NULL, cached bigint NOT NULL, forwarded bigint NOT NULL, authoritative bigint NOT NULL, dropped bigint NOT NULL,
	rc_noerror bigint, rc_nxdomain bigint, rc_servfail bigint, rc_refused bigint, rc_other bigint,
	udp bigint, tcp bigint, dot bigint, doh bigint, doq bigint,
	lat_sum_us bigint, lat_cnt bigint,
	lat_b0 bigint, lat_b1 bigint, lat_b2 bigint, lat_b3 bigint, lat_b4 bigint, lat_b5 bigint, lat_b6 bigint, lat_b7 bigint, lat_b8 bigint, lat_b9 bigint, lat_b10 bigint, lat_b11 bigint,
	PRIMARY KEY (ts, node)
);
CREATE TABLE rollup_1d (
	ts bigint NOT NULL, node integer NOT NULL,
	total bigint NOT NULL, blocked bigint NOT NULL, cached bigint NOT NULL, forwarded bigint NOT NULL, authoritative bigint NOT NULL, dropped bigint NOT NULL,
	rc_noerror bigint, rc_nxdomain bigint, rc_servfail bigint, rc_refused bigint, rc_other bigint,
	udp bigint, tcp bigint, dot bigint, doh bigint, doq bigint,
	lat_sum_us bigint, lat_cnt bigint,
	lat_b0 bigint, lat_b1 bigint, lat_b2 bigint, lat_b3 bigint, lat_b4 bigint, lat_b5 bigint, lat_b6 bigint, lat_b7 bigint, lat_b8 bigint, lat_b9 bigint, lat_b10 bigint, lat_b11 bigint,
	PRIMARY KEY (ts, node)
);
CREATE TABLE rollup_client_1m (
	ts bigint NOT NULL, node integer NOT NULL, client bigint NOT NULL, total bigint, blocked bigint, PRIMARY KEY (ts, node, client)
);
CREATE TABLE rollup_client_1h (
	ts bigint NOT NULL, node integer NOT NULL, client bigint NOT NULL, total bigint, blocked bigint, PRIMARY KEY (ts, node, client)
);
CREATE TABLE rollup_client_1d (
	ts bigint NOT NULL, node integer NOT NULL, client bigint NOT NULL, total bigint, blocked bigint, PRIMARY KEY (ts, node, client)
);
CREATE TABLE rollup_qtype_1m (
	ts bigint NOT NULL, node integer NOT NULL, qtype integer NOT NULL, total bigint, PRIMARY KEY (ts, node, qtype)
);
CREATE TABLE rollup_qtype_1h (
	ts bigint NOT NULL, node integer NOT NULL, qtype integer NOT NULL, total bigint, PRIMARY KEY (ts, node, qtype)
);
CREATE TABLE rollup_upstream_1m (
	ts bigint NOT NULL, node integer NOT NULL, upstream integer NOT NULL, total bigint, errors bigint, timeouts bigint, lat_sum_us bigint, lat_cnt bigint,
	lat_b0 bigint, lat_b1 bigint, lat_b2 bigint, lat_b3 bigint, lat_b4 bigint, lat_b5 bigint, lat_b6 bigint, lat_b7 bigint, lat_b8 bigint, lat_b9 bigint, lat_b10 bigint, lat_b11 bigint,
	PRIMARY KEY (ts, node, upstream)
);
CREATE TABLE rollup_upstream_1h (
	ts bigint NOT NULL, node integer NOT NULL, upstream integer NOT NULL, total bigint, errors bigint, timeouts bigint, lat_sum_us bigint, lat_cnt bigint,
	lat_b0 bigint, lat_b1 bigint, lat_b2 bigint, lat_b3 bigint, lat_b4 bigint, lat_b5 bigint, lat_b6 bigint, lat_b7 bigint, lat_b8 bigint, lat_b9 bigint, lat_b10 bigint, lat_b11 bigint,
	PRIMARY KEY (ts, node, upstream)
);
CREATE TABLE rollup_domain_1h (
	ts bigint NOT NULL, node integer NOT NULL, domain bigint NOT NULL, total bigint, blocked bigint, PRIMARY KEY (ts, node, domain)
);
CREATE TABLE rollup_domain_1d (
	ts bigint NOT NULL, node integer NOT NULL, domain bigint NOT NULL, total bigint, blocked bigint, PRIMARY KEY (ts, node, domain)
);
CREATE TABLE rollup_list_1h (
	ts bigint NOT NULL, node integer NOT NULL, list integer NOT NULL, hits bigint, PRIMARY KEY (ts, node, list)
);
CREATE TABLE rollup_zone_1h (
	ts bigint NOT NULL, node integer NOT NULL, zone integer NOT NULL, total bigint, PRIMARY KEY (ts, node, zone)
);
CREATE INDEX rollup_domain_1h_ts ON rollup_domain_1h (ts);
