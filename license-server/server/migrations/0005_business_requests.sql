-- 0005_business_requests: business license requests from the kudurru.io
-- form (README, "Business requests"; services/businessRequests.ts). The
-- form posts them to POST /api/v1/public/business-requests; each one is a
-- row here at once (no confirmation link: nothing is signed until a license
-- admin issues a license), the requester gets a "we have your request" mail
-- and the approvers a mail that points to the Requests page. A license
-- admin then issues a license from the request (status issued, with the
-- license) or closes it with a note (status closed). The rate limits of the
-- form count these rows; ip and user_agent go after 90 days (jobs.ts).

create table business_requests (
  id text primary key check (id ~ '^brq_[A-Za-z0-9_-]+$'),
  name text not null check (name <> ''),
  email text not null check (email = lower(btrim(email)) and email <> ''),
  company text not null check (company <> ''),
  -- The kudurru.io editions: core is issued as plan business, enterprise as plan enterprise.
  edition text not null check (edition in ('core', 'enterprise')),
  nodes integer not null check (nodes between 1 and 100000),
  details text not null default '',
  ip inet null,
  user_agent text null,
  status text not null default 'open' check (status in ('open', 'issued', 'closed')),
  decided_by text null references users (id) on delete set null,
  decided_by_label text not null default '',
  decided_at timestamptz null,
  decision_note text not null default '',
  resulting_license_id text null references licenses (id) on delete set null,
  created_at timestamptz not null default now(),
  check ((status = 'open') = (decided_at is null))
);
create index business_requests_status_created_at_idx on business_requests (status, created_at);
create index business_requests_email_created_at_idx on business_requests (email, created_at);
create index business_requests_ip_created_at_idx on business_requests (ip, created_at);
create index business_requests_decided_by_idx on business_requests (decided_by);
create index business_requests_resulting_license_id_idx on business_requests (resulting_license_id);
comment on table business_requests is 'Business license requests from the kudurru.io form: open until a license admin issues a license from one (issued) or closes it (closed). Rate limits count these rows; ip and user_agent go after 90 days.';

-- The runtime role's grants, as 0002 does: only when the role exists.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'license_portal_app') then
    grant select, insert, update on business_requests to license_portal_app;
  end if;
end
$$;
