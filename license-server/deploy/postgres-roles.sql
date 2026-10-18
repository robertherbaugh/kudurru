-- One-time database setup for the kudurru license portal: the two roles of
-- the least-privilege setup (README, "Database roles"). Run it once as an
-- administrator, over a direct connection, BEFORE the first migration; on
-- PlanetScale that is the default `postgres` role on port 5432, e.g.
--
--   psql "postgres://postgres:…@<host>:5432/<database>?sslmode=verify-full" \
--     -v db=<database> -v owner_password='…' -v app_password='…' \
--     -f license-server/deploy/postgres-roles.sql
--
-- Then migrate as the owner (it creates the tables and, through migrations
-- 0002 to 0006, grants the runtime role exactly what the server uses) and give the
-- serving container only the runtime role:
--
--   DATABASE_MIGRATE_URL=postgres://license_portal_owner:…@<host>:5432/<db>   (migrate step only)
--   DATABASE_URL=postgres://license_portal_app:…@<host>:5432/<db>             (the server)
--
-- CREATE ROLE's defaults are NOSUPERUSER NOCREATEDB NOCREATEROLE
-- NOREPLICATION NOBYPASSRLS; nothing here widens them.

\set ON_ERROR_STOP on

-- The migration owner: owns every table, function and trigger; used only by
-- `bun dist/server.js migrate` (CI, a workstation or a one-off container).
create role license_portal_owner login password :'owner_password';

-- The runtime role: the only credentials the serving container holds.
create role license_portal_app login password :'app_password';

grant connect on database :"db" to license_portal_owner, license_portal_app;
grant usage, create on schema public to license_portal_owner;
grant usage on schema public to license_portal_app;

-- If the runtime role is created after the migrations already ran, run this
-- block as license_portal_owner once (migrations 0002 to 0006 do the same
-- on a fresh database; server/test/roles.test.ts keeps the two in step):
--
-- grant select on schema_migrations to license_portal_app;
-- grant select, insert, update on users, customers, licenses, license_keys, license_requests to license_portal_app;
-- grant select, insert, update, delete on sessions, customer_members, public_key_requests to license_portal_app;
-- grant select, insert, delete on oidc_logins, blocked_emails to license_portal_app;
-- grant select, insert on audit_events to license_portal_app;
-- grant usage on sequence audit_events_id_seq to license_portal_app;
-- grant select, insert, update, delete on request_approval_links to license_portal_app;
-- grant delete on users to license_portal_app;
-- grant select, insert, update on business_requests to license_portal_app;
-- grant select, insert, update on order_forms to license_portal_app;
-- grant usage on sequence order_form_number_seq to license_portal_app;
