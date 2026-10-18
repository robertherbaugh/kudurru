-- 0001: the core tables (dev-docs/design-record.md §4.5, D158).

-- One row per local file that ships to this database: a node's audit
-- spool, state.db or querylog.db. The uuid is minted with the file, so a
-- replaced emptyDir starts a new source instead of colliding with the old
-- one; node is an attribute, not part of the key.
CREATE TABLE source (
	id         integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	uuid       text        NOT NULL UNIQUE,
	kind       text        NOT NULL,
	node       text        NOT NULL,
	version    text        NOT NULL DEFAULT '',
	first_seen timestamptz NOT NULL DEFAULT now(),
	last_seen  timestamptz NOT NULL DEFAULT now()
);

-- Where each source's shipper is: written in the same transaction as the
-- rows it covers, so a retry after any failure resumes exactly.
CREATE TABLE ship_cursor (
	source     integer     NOT NULL REFERENCES source(id) ON DELETE CASCADE,
	stream     text        NOT NULL,
	last_id    bigint      NOT NULL DEFAULT 0,
	last_ts    timestamptz,
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (source, stream)
);
