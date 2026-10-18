-- 0006_order_forms: Order Forms, Schedule A of the commercial agreement
-- (README, "Order Forms"; services/orderForms.ts). A license admin drafts a
-- form for a customer (often from a business request), sends it (its text
-- is frozen and its SHA-256 stored), an authorized contact of the customer
-- accepts it signed in (customer_acceptance: name, title, address, time,
-- client and the hash accepted; document_text keeps the text sent, so the
-- hash stays verifiable whatever later renderings look like) or declines it, and a license admin
-- countersigns it (licensor_acceptance): executed, final. One license may
-- be issued from an executed form (license_id). Forms are never deleted;
-- void records a withdrawal before execution. Numbers come from
-- order_form_number_seq: OF-<year>-<number, at least four digits>.

create sequence order_form_number_seq;

create table order_forms (
  id text primary key check (id ~ '^ord_[A-Za-z0-9_-]+$'),
  number text not null unique check (number ~ '^OF-[0-9]{4}-[0-9]{4,}$'),
  customer_id text not null references customers (id),
  business_request_id text null references business_requests (id) on delete set null,
  status text not null default 'draft' check (status in ('draft', 'sent', 'accepted', 'executed', 'declined', 'void')),
  -- 1 for the first text sent; each revision after a send adds one.
  version integer not null default 1 check (version >= 1),
  -- OrderFormTerms (shared/api.ts), validated by the server.
  terms jsonb not null check (jsonb_typeof(terms) = 'object'),
  document_sha256 text null check (document_sha256 ~ '^[0-9a-f]{64}$'),
  -- The Markdown text sent, exactly as hashed (shared/orderForms.ts orderFormDocument).
  document_text text null,
  sent_at timestamptz null,
  sent_by text null references users (id) on delete set null,
  sent_by_label text not null default '',
  customer_acceptance jsonb null check (customer_acceptance is null or jsonb_typeof(customer_acceptance) = 'object'),
  licensor_acceptance jsonb null check (licensor_acceptance is null or jsonb_typeof(licensor_acceptance) = 'object'),
  declined_at timestamptz null,
  declined_by_label text not null default '',
  decline_note text not null default '',
  voided_at timestamptz null,
  voided_by text null references users (id) on delete set null,
  voided_by_label text not null default '',
  void_reason text not null default '',
  license_id text null unique references licenses (id) on delete set null,
  created_at timestamptz not null default now(),
  created_by text null references users (id) on delete set null,
  created_by_label text not null default '',
  updated_at timestamptz not null default now(),
  check ((status = 'draft') = (document_sha256 is null and sent_at is null) or status = 'void'),
  check ((status in ('accepted', 'executed')) = (customer_acceptance is not null) or status = 'void'),
  check ((status = 'executed') = (licensor_acceptance is not null)),
  check ((status = 'declined') = (declined_at is not null) or status = 'void'),
  check ((document_text is null) = (document_sha256 is null)),
  check ((status = 'void') = (voided_at is not null)),
  check (license_id is null or status = 'executed')
);
create index order_forms_status_created_at_idx on order_forms (status, created_at);
create index order_forms_customer_id_idx on order_forms (customer_id, created_at);
create index order_forms_business_request_id_idx on order_forms (business_request_id);
create index order_forms_sent_by_idx on order_forms (sent_by);
create index order_forms_voided_by_idx on order_forms (voided_by);
create index order_forms_created_by_idx on order_forms (created_by);
comment on table order_forms is 'Order Forms (Schedule A of the commercial agreement): draft, sent (text frozen, hashed), accepted by an authorized contact, executed once countersigned; declined or void otherwise. Never deleted.';

-- The runtime role's grants, as 0002 does: only when the role exists.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'license_portal_app') then
    grant select, insert, update on order_forms to license_portal_app;
    grant usage on sequence order_form_number_seq to license_portal_app;
  end if;
end
$$;
