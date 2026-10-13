-- 0003_request_approval_links: the one-click approval links of the admin
-- mail (README, "The kudurru.io form"; services/approvals.ts). When a
-- personal request waits for approval, every recipient of the admin mail
-- gets a link of their own, ${PUBLIC_URL}/approve?token=<token>: the token is
-- 32 random bytes (base64url) and only its SHA-256 is stored, so a database
-- dump approves nothing. A link works APPROVAL_LINK_TTL_DAYS (7) days and
-- once (used_at); a request decided another way makes its links useless
-- (the decision checks that the request is still open). The cleanup job
-- deletes links 30 days after they expired.

create table request_approval_links (
  token_hash bytea primary key,
  request_id text not null references license_requests (id) on delete cascade,
  sent_to text not null check (sent_to = lower(sent_to)),
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz
);
create index request_approval_links_request_id_idx on request_approval_links (request_id);
create index request_approval_links_expires_at_idx on request_approval_links (expires_at);
comment on table request_approval_links is 'One-click approval links mailed to the approvers of a personal request: SHA-256 of the token, the address it went to, 7 days, single use. Deleted 30 days after expiry.';

comment on table license_requests is 'Personal key requests that wait for an admin (approval required, the default; a blocked email; a withdrawn license in the history; the daily cap of automatic keys). At most one open request per email.';

-- The runtime role's grants, as 0002 does: only when the role exists.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'license_portal_app') then
    grant select, insert, update, delete on request_approval_links to license_portal_app;
  end if;
end
$$;
