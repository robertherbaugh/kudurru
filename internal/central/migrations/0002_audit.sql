-- 0002: the audit log of every node (D159).

-- One row per event. id is the position in the one hash chain of the
-- database, assigned under the audit_chain row lock, so ids are gapless
-- and commit-ordered (a keyset cursor never skips an event). The hashed
-- fields are stored exactly: documents as text, never jsonb, which would
-- reorder their keys. origin_* keep the hash chain of the node's own file.
CREATE TABLE audit (
	id               bigint      PRIMARY KEY,
	ts               timestamptz NOT NULL,
	node             text        NOT NULL,
	source           integer     NOT NULL REFERENCES source(id),
	source_id        bigint      NOT NULL,
	actor            text        NOT NULL,
	actor_kind       text        NOT NULL DEFAULT '',
	actor_id         text        NOT NULL DEFAULT '',
	actor_name       text        NOT NULL DEFAULT '',
	actor_groups     text[]      NOT NULL DEFAULT '{}',
	actor_auth       text        NOT NULL DEFAULT '',
	action           text        NOT NULL,
	kind             text        NOT NULL,
	namespace        text        NOT NULL DEFAULT '',
	name             text        NOT NULL DEFAULT '',
	result           text        NOT NULL,
	reason           text        NOT NULL DEFAULT '',
	authz            text        NOT NULL DEFAULT '',
	manager          text        NOT NULL DEFAULT '',
	resource_version text        NOT NULL DEFAULT '',
	before           text,
	after            text,
	req_id           text        NOT NULL DEFAULT '',
	req_ip           text        NOT NULL DEFAULT '',
	req_ua           text        NOT NULL DEFAULT '',
	req_method       text        NOT NULL DEFAULT '',
	req_path         text        NOT NULL DEFAULT '',
	origin_prev_hash text        NOT NULL DEFAULT '',
	origin_hash      text        NOT NULL DEFAULT '',
	prev_hash        text        NOT NULL,
	hash             text        NOT NULL,
	UNIQUE (source, source_id)
);
CREATE INDEX audit_kind_name ON audit (kind, name, id);
CREATE INDEX audit_actor ON audit (actor, id);
CREATE INDEX audit_actor_name ON audit (actor_name, id);
CREATE INDEX audit_ts ON audit (ts);

-- The head of the chain (one row): every insert locks it, so events from
-- all nodes form one sequence. pruned_* anchor the chain after retention.
CREATE TABLE audit_chain (
	one            boolean PRIMARY KEY DEFAULT true CHECK (one),
	seq            bigint  NOT NULL DEFAULT 0,
	hash           text    NOT NULL DEFAULT '',
	pruned_through bigint  NOT NULL DEFAULT 0,
	pruned_hash    text    NOT NULL DEFAULT ''
);
INSERT INTO audit_chain DEFAULT VALUES;
