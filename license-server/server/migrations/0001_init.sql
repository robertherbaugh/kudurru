-- 0001_init: the license portal's schema (design spec §4).
--
-- Applied by server/db/migrate.ts in one transaction, after it created
-- schema_migrations (version int primary key, name text, applied_at
-- timestamptz) itself. Emails are stored lower-cased and trimmed; there is
-- no citext. Ids are prefixed text (usr_, cus_, lic_, key_, req_, pkr_);
-- a license id is also its keys' jti, and imported keys keep their own jti.
-- Instants are timestamptz. A key's token is authoritative: the claims jsonb
-- next to it is an audit snapshot and is never re-signed.
--
-- Production runs two roles (README, roles.sql): the owner applies this
-- file; the runtime role gets DML on the tables, only SELECT and INSERT on
-- audit_events, USAGE on its sequence, and no TRUNCATE or DDL. The triggers
-- at the end refuse UPDATE, DELETE and TRUNCATE on audit_events even for
-- the owner.

-- People who signed in through the IdP, keyed by (issuer, subject).
create table users (
  id text primary key check (id ~ '^usr_[A-Za-z0-9_-]+$'),
  issuer text not null check (issuer <> ''),
  subject text not null check (subject <> ''),
  -- Kept only when the IdP says it is verified.
  email text null check (email = lower(btrim(email)) and email <> ''),
  email_verified boolean not null default false,
  name text not null default '',
  idp_roles text[] not null default '{}',
  granted_roles text[] not null default '{}',
  disabled_at timestamptz null,
  created_at timestamptz not null default now(),
  last_login_at timestamptz null,
  unique (issuer, subject),
  check (idp_roles <@ '{customer,license_admin,superadmin}'::text[] and granted_roles <@ '{customer,license_admin,superadmin}'::text[])
);
create index users_email_idx on users (email);
comment on table users is 'People who signed in through the IdP, keyed by (issuer, subject). Effective roles = idp_roles ∪ granted_roles; none while disabled_at is set.';

-- Server-side sessions; the cookie holds the secret, the row its SHA-256.
create table sessions (
  id_hash bytea primary key,
  user_id text not null references users (id) on delete cascade,
  csrf text not null,
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  idle_expires_at timestamptz not null,
  expires_at timestamptz not null,
  auth_time timestamptz not null,
  ip inet null,
  user_agent text null,
  check (idle_expires_at <= expires_at)
);
create index sessions_user_id_idx on sessions (user_id);
create index sessions_expires_at_idx on sessions (expires_at);
create index sessions_idle_expires_at_idx on sessions (idle_expires_at);
comment on table sessions is 'Signed-in sessions: SHA-256 of the cookie secret, the CSRF token, idle and absolute expiry. Deleted at logout, on role or disable changes and when expired.';

-- Sign-ins in flight (state, nonce, PKCE verifier), single use, 10 minutes.
create table oidc_logins (
  state text primary key,
  binding_hash bytea not null,
  nonce text not null,
  code_verifier text not null,
  next text not null default '/',
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);
create index oidc_logins_expires_at_idx on oidc_logins (expires_at);
comment on table oidc_logins is 'OIDC sign-ins in flight: state, nonce and PKCE verifier, bound to the login cookie by binding_hash. Deleted when the callback consumes them or when expired.';

-- Who licenses belong to: a person or an organization.
create table customers (
  id text primary key check (id ~ '^cus_[A-Za-z0-9_-]+$'),
  name text not null check (btrim(name) <> ''),
  kind text not null check (kind in ('person', 'organization')),
  contact_email text null check (contact_email = lower(btrim(contact_email)) and contact_email <> ''),
  notes text not null default '',
  created_at timestamptz not null default now(),
  created_by text null references users (id) on delete set null,
  created_by_label text not null default '',
  updated_at timestamptz not null default now()
);
create index customers_kind_created_at_idx on customers (kind, created_at);
create index customers_created_by_idx on customers (created_by);
create index customers_contact_email_idx on customers (contact_email);
comment on table customers is 'Who licenses belong to: a person (personal keys) or an organization. Customers see their licenses through customer_members.';

-- The people with access to a customer's licenses: an invite until a user redeems it.
create table customer_members (
  customer_id text not null references customers (id) on delete cascade,
  email text not null check (email = lower(btrim(email)) and email <> ''),
  user_id text null references users (id) on delete set null,
  invite_token_hash bytea null unique,
  invite_expires_at timestamptz null,
  invited_at timestamptz not null default now(),
  invited_by_label text not null default '',
  linked_at timestamptz null,
  primary key (customer_id, email)
);
create unique index customer_members_customer_user_uq on customer_members (customer_id, user_id) where user_id is not null;
create index customer_members_email_idx on customer_members (email);
create index customer_members_user_id_idx on customer_members (user_id);
comment on table customer_members is 'People with access to a customer''s licenses. invited: a hashed invite token; linked: user_id set by redeeming it (or MEMBER_AUTO_LINK). A linked row is never re-linked.';

-- Licenses: the record behind the current key.
create table licenses (
  -- = the jti of every key of the license (lic_ + base64url of 9 bytes; imported keys keep theirs).
  id text primary key check (id <> '' and length(id) <= 200),
  customer_id text not null references customers (id) on delete restrict,
  licensee text not null check (btrim(licensee) <> ''),
  plan text not null check (plan in ('personal', 'business', 'enterprise', 'trial')),
  nodes int not null check (nodes between 1 and 100000),
  -- null: every feature, including later ones; never an empty list.
  features text[] null check (features is null or (cardinality(features) > 0 and array_position(features, null) is null)),
  -- The exp of the current key.
  expires_at timestamptz not null,
  agreement_ref text not null default '',
  notes text not null default '',
  status text not null default 'active' check (status in ('active', 'withdrawn')),
  withdrawn_at timestamptz null,
  withdrawn_by text null references users (id) on delete set null,
  withdrawn_by_label text not null default '',
  withdraw_reason text not null default '',
  withdraw_message text not null default '',
  attestation jsonb null check (attestation is null or jsonb_typeof(attestation) = 'object'),
  source text not null check (source in ('admin', 'web', 'portal', 'import')),
  created_at timestamptz not null default now(),
  created_by text null references users (id) on delete set null,
  created_by_label text not null default '',
  updated_at timestamptz not null default now(),
  check ((status = 'withdrawn') = (withdrawn_at is not null))
);
create index licenses_customer_id_idx on licenses (customer_id);
create index licenses_expires_at_idx on licenses (expires_at);
create index licenses_status_plan_idx on licenses (status, plan);
create index licenses_created_at_idx on licenses (created_at);
create index licenses_created_by_idx on licenses (created_by);
create index licenses_withdrawn_by_idx on licenses (withdrawn_by);
comment on table licenses is 'One row per license; id = the jti of its keys. expires_at is the exp of the current key. Withdrawn licenses keep their keys working on nodes until the latest exp + 30 days; the portal only stops handing them out.';

-- Every signed key; exactly one current key per license.
create table license_keys (
  id text primary key check (id ~ '^key_[A-Za-z0-9_-]+$'),
  license_id text not null references licenses (id) on delete cascade,
  kid text not null,
  -- The compact JWS as signed: authoritative.
  token text not null,
  -- Audit snapshot of the signed claims; never re-signed.
  claims jsonb not null check (jsonb_typeof(claims) = 'object'),
  issued_at timestamptz not null,
  expires_at timestamptz not null,
  issued_by text null references users (id) on delete set null,
  issued_by_label text not null default '',
  reason text not null check (reason in ('issue', 'renew', 'change', 'import')),
  superseded_at timestamptz null,
  superseded_by text null references license_keys (id),
  check (expires_at > issued_at),
  check ((superseded_by is null) or (superseded_at is not null))
);
create unique index license_keys_current_uq on license_keys (license_id) where superseded_at is null;
create index license_keys_license_id_issued_at_idx on license_keys (license_id, issued_at desc);
create index license_keys_kid_idx on license_keys (kid);
create index license_keys_issued_by_issued_at_idx on license_keys (issued_by, issued_at);
create index license_keys_superseded_by_idx on license_keys (superseded_by);
comment on table license_keys is 'Every key the portal signed or imported. The unsuperseded key is the license''s current key (one per license, license_keys_current_uq); the token is authoritative and never appears in audit details or logs.';

-- Personal key requests that wait for a person.
create table license_requests (
  id text primary key check (id ~ '^req_[A-Za-z0-9_-]+$'),
  kind text not null default 'personal' check (kind = 'personal'),
  source text not null check (source in ('web', 'portal')),
  requested_by text null references users (id) on delete set null,
  requester_email text not null check (requester_email = lower(btrim(requester_email)) and requester_email <> ''),
  name text not null,
  use_description text not null,
  attestation jsonb not null check (jsonb_typeof(attestation) = 'object'),
  review_reason text null check (review_reason in ('auto_approve_off', 'daily_cap', 'withdrawn_history', 'blocked')),
  status text not null default 'open' check (status in ('open', 'approved', 'rejected', 'canceled')),
  decided_by text null references users (id) on delete set null,
  decided_by_label text not null default '',
  decided_at timestamptz null,
  decision_note text not null default '',
  resulting_license_id text null references licenses (id) on delete set null,
  resulting_key_id text null references license_keys (id) on delete set null,
  created_at timestamptz not null default now(),
  check ((status = 'open') = (decided_at is null))
);
-- One open request per address: a second one while it waits is the same request (pending_review).
create unique index license_requests_open_email_uq on license_requests (requester_email) where status = 'open';
create index license_requests_status_created_at_idx on license_requests (status, created_at);
create index license_requests_requester_email_idx on license_requests (requester_email, created_at);
create index license_requests_requested_by_idx on license_requests (requested_by);
create index license_requests_decided_by_idx on license_requests (decided_by);
create index license_requests_resulting_license_id_idx on license_requests (resulting_license_id);
create index license_requests_resulting_key_id_idx on license_requests (resulting_key_id);
comment on table license_requests is 'Personal key requests that wait for review (blocked email, withdrawn history, auto-approve off or the daily cap). At most one open request per email.';

-- The kudurru.io form: one row per submission, confirmed by a mailed link.
create table public_key_requests (
  id text primary key check (id ~ '^pkr_[A-Za-z0-9_-]+$'),
  name text not null,
  email text not null check (email = lower(btrim(email)) and email <> ''),
  use_description text not null,
  noncommercial_attested_at timestamptz not null,
  ip inet null,
  user_agent text null,
  token_hash bytea not null unique,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  verified_at timestamptz null,
  outcome text null check (outcome in ('issued', 'renewed', 'resent', 'pending_review')),
  license_request_id text null references license_requests (id) on delete set null,
  license_id text null references licenses (id) on delete set null
);
create index public_key_requests_email_created_at_idx on public_key_requests (email, created_at);
create index public_key_requests_ip_created_at_idx on public_key_requests (ip, created_at);
create index public_key_requests_expires_at_idx on public_key_requests (expires_at);
create index public_key_requests_license_request_id_idx on public_key_requests (license_request_id);
create index public_key_requests_license_id_idx on public_key_requests (license_id);
comment on table public_key_requests is 'Submissions of the kudurru.io form. The confirmation link holds the token; the row keeps its SHA-256. Rate limits count these rows; unverified rows go 7 days after expiry, ip and user_agent after 90 days.';

-- Addresses whose personal requests always wait for review.
create table blocked_emails (
  email text primary key check (email = lower(btrim(email)) and email <> ''),
  reason text not null default '',
  blocked_by text null references users (id) on delete set null,
  blocked_by_label text not null default '',
  blocked_at timestamptz not null default now()
);
create index blocked_emails_blocked_by_idx on blocked_emails (blocked_by);
comment on table blocked_emails is 'Addresses whose personal key requests always wait for review (written when a license is withdrawn with "block personal").';

-- The append-only audit log.
create table audit_events (
  id bigserial primary key,
  at timestamptz not null default now(),
  -- No foreign key: the log outlives what it names.
  actor_id text null,
  actor_label text not null default '',
  action text not null,
  target_type text not null,
  target_id text not null,
  details jsonb not null default '{}' check (jsonb_typeof(details) = 'object'),
  ip inet null,
  user_agent text null,
  request_id text null
);
create index audit_events_at_idx on audit_events (at desc);
create index audit_events_target_idx on audit_events (target_type, target_id, id desc);
create index audit_events_actor_id_idx on audit_events (actor_id, id desc);
create index audit_events_action_idx on audit_events (action, id desc);
comment on table audit_events is 'Append-only audit log: every change and every key download. UPDATE, DELETE and TRUNCATE are refused by triggers; details never contain a key token.';

create function audit_events_append_only() returns trigger
language plpgsql as $$
begin
  raise exception 'audit_events is append-only (% refused)', tg_op
    using errcode = 'insufficient_privilege';
end
$$;
comment on function audit_events_append_only() is 'Refuses UPDATE, DELETE and TRUNCATE on audit_events.';

create trigger audit_events_no_update_delete
  before update or delete on audit_events
  for each row execute function audit_events_append_only();

create trigger audit_events_no_truncate
  before truncate on audit_events
  for each statement execute function audit_events_append_only();
