-- Least-privilege grants for the runtime role (design spec §14; README,
-- "Database roles"). Production runs with two roles: license_portal_owner
-- owns every object and is used only by `server.js migrate`;
-- license_portal_app is the only role the serving container holds. This
-- migration grants the runtime role exactly what the server's SQL uses,
-- when that role exists (it is created once by deploy/postgres-roles.sql);
-- on a database without it (dev, tests, a single-role setup) it does
-- nothing. A later migration that adds a table adds its grants the same way.
--
-- What the runtime role cannot do: DDL, TRUNCATE, own anything; on
-- audit_events only read and append (the triggers also refuse UPDATE,
-- DELETE and TRUNCATE for every role); never delete a license, a key or a
-- user; only read schema_migrations.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'license_portal_app') then
    grant select on schema_migrations to license_portal_app;
    grant select, insert, update on users, customers, licenses, license_keys, license_requests to license_portal_app;
    grant select, insert, update, delete on sessions, customer_members, public_key_requests to license_portal_app;
    grant select, insert, delete on oidc_logins, blocked_emails to license_portal_app;
    grant select, insert on audit_events to license_portal_app;
    grant usage on sequence audit_events_id_seq to license_portal_app;
  end if;
end
$$;
