-- 0004_user_delete: a superadmin may delete a user (README, "Users";
-- services/users.ts deleteUser). The user's sessions go with the row (on
-- delete cascade) and their customer memberships are deleted first, so an
-- invite-free member row can never re-link to whoever signs in next with
-- that address. Everything else that names the user keeps a label and loses
-- the link (on delete set null: customers, licenses, keys, requests, the
-- blocklist); the audit trail has no foreign key and keeps the id, and the
-- user.deleted event records the issuer, subject and email. A person who
-- signs in again gets a new user without roles: disable a user to keep
-- them out.

comment on table users is 'People who signed in through the IdP, keyed by (issuer, subject). Effective roles = idp_roles ∪ granted_roles; none while disabled_at is set. A superadmin may delete a user; signing in again creates a new one.';

-- The runtime role's grants, as 0002 does: only when the role exists.
-- Postgres runs the foreign key actions (cascade, set null) as the owner of
-- the referencing tables, so deleting a user needs no other grant.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'license_portal_app') then
    grant delete on users to license_portal_app;
  end if;
end
$$;
