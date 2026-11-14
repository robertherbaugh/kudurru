-- 0004: safe reclamation of unreferenced central dictionaries.
-- Existing IDs and identity sequences are preserved. Validate every reference:
-- an invalid pre-upgrade image must fail atomically rather than lose data.
-- Older shippers cannot revalidate cached IDs after reclamation; compat is 4.
-- The FK backstop also refuses stale inserts from an already-running old node.

CREATE INDEX query_node_ref ON query (node);
ALTER TABLE query ADD CONSTRAINT query_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
ALTER TABLE query ADD CONSTRAINT query_client_dict_fk FOREIGN KEY (client) REFERENCES dict_client (id);
ALTER TABLE query ADD CONSTRAINT query_domain_dict_fk FOREIGN KEY (domain) REFERENCES dict_domain (id);
CREATE INDEX query_cname_ref ON query (cname);
ALTER TABLE query ADD CONSTRAINT query_cname_dict_fk FOREIGN KEY (cname) REFERENCES dict_domain (id);
CREATE INDEX query_upstream_ref ON query (upstream);
ALTER TABLE query ADD CONSTRAINT query_upstream_dict_fk FOREIGN KEY (upstream) REFERENCES dict_upstream (id);
CREATE INDEX query_zone_ref ON query (zone);
ALTER TABLE query ADD CONSTRAINT query_zone_dict_fk FOREIGN KEY (zone) REFERENCES dict_zone (id);
CREATE INDEX query_list_ref ON query (list);
ALTER TABLE query ADD CONSTRAINT query_list_dict_fk FOREIGN KEY (list) REFERENCES dict_list (id);
CREATE INDEX query_policy_ref ON query (policy);
ALTER TABLE query ADD CONSTRAINT query_policy_dict_fk FOREIGN KEY (policy) REFERENCES dict_policy (id);
CREATE INDEX rollup_1m_node_ref ON rollup_1m (node);
ALTER TABLE rollup_1m ADD CONSTRAINT rollup_1m_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1m_node_ref ON rollup_client_1m (node);
ALTER TABLE rollup_client_1m ADD CONSTRAINT rollup_client_1m_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1m_client_ref ON rollup_client_1m (client);
ALTER TABLE rollup_client_1m ADD CONSTRAINT rollup_client_1m_client_dict_fk FOREIGN KEY (client) REFERENCES dict_client (id);
CREATE INDEX rollup_qtype_1m_node_ref ON rollup_qtype_1m (node);
ALTER TABLE rollup_qtype_1m ADD CONSTRAINT rollup_qtype_1m_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_upstream_1m_node_ref ON rollup_upstream_1m (node);
ALTER TABLE rollup_upstream_1m ADD CONSTRAINT rollup_upstream_1m_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_upstream_1m_upstream_ref ON rollup_upstream_1m (upstream);
ALTER TABLE rollup_upstream_1m ADD CONSTRAINT rollup_upstream_1m_upstream_dict_fk FOREIGN KEY (upstream) REFERENCES dict_upstream (id);
CREATE INDEX rollup_1h_node_ref ON rollup_1h (node);
ALTER TABLE rollup_1h ADD CONSTRAINT rollup_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1h_node_ref ON rollup_client_1h (node);
ALTER TABLE rollup_client_1h ADD CONSTRAINT rollup_client_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1h_client_ref ON rollup_client_1h (client);
ALTER TABLE rollup_client_1h ADD CONSTRAINT rollup_client_1h_client_dict_fk FOREIGN KEY (client) REFERENCES dict_client (id);
CREATE INDEX rollup_qtype_1h_node_ref ON rollup_qtype_1h (node);
ALTER TABLE rollup_qtype_1h ADD CONSTRAINT rollup_qtype_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_upstream_1h_node_ref ON rollup_upstream_1h (node);
ALTER TABLE rollup_upstream_1h ADD CONSTRAINT rollup_upstream_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_upstream_1h_upstream_ref ON rollup_upstream_1h (upstream);
ALTER TABLE rollup_upstream_1h ADD CONSTRAINT rollup_upstream_1h_upstream_dict_fk FOREIGN KEY (upstream) REFERENCES dict_upstream (id);
CREATE INDEX rollup_domain_1h_node_ref ON rollup_domain_1h (node);
ALTER TABLE rollup_domain_1h ADD CONSTRAINT rollup_domain_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_domain_1h_domain_ref ON rollup_domain_1h (domain);
ALTER TABLE rollup_domain_1h ADD CONSTRAINT rollup_domain_1h_domain_dict_fk FOREIGN KEY (domain) REFERENCES dict_domain (id);
CREATE INDEX rollup_list_1h_node_ref ON rollup_list_1h (node);
ALTER TABLE rollup_list_1h ADD CONSTRAINT rollup_list_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_list_1h_list_ref ON rollup_list_1h (list);
ALTER TABLE rollup_list_1h ADD CONSTRAINT rollup_list_1h_list_dict_fk FOREIGN KEY (list) REFERENCES dict_list (id);
CREATE INDEX rollup_zone_1h_node_ref ON rollup_zone_1h (node);
ALTER TABLE rollup_zone_1h ADD CONSTRAINT rollup_zone_1h_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_zone_1h_zone_ref ON rollup_zone_1h (zone);
ALTER TABLE rollup_zone_1h ADD CONSTRAINT rollup_zone_1h_zone_dict_fk FOREIGN KEY (zone) REFERENCES dict_zone (id);
CREATE INDEX rollup_1d_node_ref ON rollup_1d (node);
ALTER TABLE rollup_1d ADD CONSTRAINT rollup_1d_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1d_node_ref ON rollup_client_1d (node);
ALTER TABLE rollup_client_1d ADD CONSTRAINT rollup_client_1d_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_client_1d_client_ref ON rollup_client_1d (client);
ALTER TABLE rollup_client_1d ADD CONSTRAINT rollup_client_1d_client_dict_fk FOREIGN KEY (client) REFERENCES dict_client (id);
CREATE INDEX rollup_domain_1d_node_ref ON rollup_domain_1d (node);
ALTER TABLE rollup_domain_1d ADD CONSTRAINT rollup_domain_1d_node_dict_fk FOREIGN KEY (node) REFERENCES dict_node (id);
CREATE INDEX rollup_domain_1d_domain_ref ON rollup_domain_1d (domain);
ALTER TABLE rollup_domain_1d ADD CONSTRAINT rollup_domain_1d_domain_dict_fk FOREIGN KEY (domain) REFERENCES dict_domain (id);

-- Keyset sweeps have a fixed upper ID so continuous ingestion cannot starve
-- skipped low IDs. Cursor advancement and deletions commit together.
INSERT INTO kudurru_meta (key, value) VALUES
    ('prune_dict_node', '{"after":0,"through":0}'),
    ('prune_dict_client', '{"after":0,"through":0}'),
    ('prune_dict_domain', '{"after":0,"through":0}'),
    ('prune_dict_upstream', '{"after":0,"through":0}'),
    ('prune_dict_zone', '{"after":0,"through":0}'),
    ('prune_dict_list', '{"after":0,"through":0}'),
    ('prune_dict_policy', '{"after":0,"through":0}');
