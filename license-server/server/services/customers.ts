// Customers (design spec §4, §8 "customers"): who licenses belong to, a
// person or an organization. The rows, the paged list, create and update,
// and the row lock every license action takes on a customer it writes to.
// This module sits below the others (members, licenses, personal and
// requests use it); the detail page's composition with members, licenses
// and requests is in api/customers.ts.

import type { ActorRef, Customer, CustomerKind, CustomerListQuery, CustomerRef, Page } from '../../shared/api';
import type { SQL } from 'bun';
import { newId } from '../db/ids';
import { maybeOne, toInt, type Queryable } from '../db/sql';
import { notFound } from '../http/errors';
import { auditCtx } from './audit';
import { actorColumns, type ServiceCtx } from './types';

/** A piece of SQL to embed in a query (Bun.sql fragment); build a fresh one per query. */
export type Fragment = SQL.Query<unknown>;

/** A customers row. */
export interface CustomerRow {
  id: string;
  name: string;
  kind: CustomerKind;
  contact_email: string | null;
  notes: string;
  created_at: Date;
  created_by: string | null;
  created_by_label: string;
  updated_at: Date;
}

/** A row with its counts (lists). */
interface CustomerListRow extends CustomerRow {
  license_count: string;
  member_count: string;
}

/** An ActorRef from a *_by / *_by_label pair. */
export function actorRef(id: string | null, label: string): ActorRef {
  return { id, label };
}

export function customerRef(row: Pick<CustomerRow, 'id' | 'name' | 'kind'>): CustomerRef {
  return { id: row.id, name: row.name, kind: row.kind };
}

/** A LIKE pattern matching `s` anywhere, with % _ and \ escaped (use with escape '\'). */
export function containsPattern(s: string): string {
  return `%${s.replace(/[\\%_]/g, (c) => `\\${c}`)}%`;
}

export function customerDto(row: CustomerRow, licenseCount: number, memberCount: number): Customer {
  return {
    id: row.id,
    name: row.name,
    kind: row.kind,
    contactEmail: row.contact_email,
    licenseCount,
    memberCount,
    createdAt: row.created_at.toISOString(),
    updatedAt: row.updated_at.toISOString(),
  };
}

/** The customer, or null. */
export async function findCustomer(q: Queryable, id: string): Promise<CustomerRow | null> {
  return maybeOne(await q<CustomerRow[]>`select * from customers where id = ${id}`);
}

/** The customer, or 404. */
export async function getCustomerRow(q: Queryable, id: string): Promise<CustomerRow> {
  const row = await findCustomer(q, id);
  if (!row) notFound('There is no such customer.');
  return row;
}

/** Locks the customer row (SELECT … FOR UPDATE) for a write that depends on it; 404 when missing. */
export async function lockCustomer(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<CustomerRow> {
  const row = maybeOne(await ctx.tx<CustomerRow[]>`select * from customers where id = ${id} for update`);
  if (!row) notFound('There is no such customer.');
  return row;
}

/** The number of licenses and members of a customer. */
export async function customerCounts(q: Queryable, id: string): Promise<{ licenses: number; members: number }> {
  const rows = await q<{ licenses: string; members: string }[]>`
    select (select count(*) from licenses where customer_id = ${id}) as licenses,
           (select count(*) from customer_members where customer_id = ${id}) as members`;
  return { licenses: toInt(rows[0]?.licenses), members: toInt(rows[0]?.members) };
}

/** The paged customer list: q matches the name, the contact email and member emails. */
export async function listCustomers(q: Queryable, query: Required<Pick<CustomerListQuery, 'limit' | 'offset'>> & Pick<CustomerListQuery, 'q'>): Promise<Page<Customer>> {
  const pattern = query.q ? containsPattern(query.q) : null;
  // A fresh fragment per query: Bun.sql fragments are not reused.
  const where = (): Fragment =>
    pattern
      ? q`where c.name ilike ${pattern} escape '\\' or c.contact_email ilike ${pattern} escape '\\'
          or exists (select 1 from customer_members m where m.customer_id = c.id and m.email ilike ${pattern} escape '\\')`
      : q``;
  const rows = await q<CustomerListRow[]>`
    select c.*,
           (select count(*) from licenses l where l.customer_id = c.id) as license_count,
           (select count(*) from customer_members m where m.customer_id = c.id) as member_count
      from customers c ${where()}
     order by lower(c.name), c.created_at, c.id
     limit ${query.limit} offset ${query.offset}`;
  const total = await q<{ n: string }[]>`select count(*) as n from customers c ${where()}`;
  return { items: rows.map((r) => customerDto(r, toInt(r.license_count), toInt(r.member_count))), total: toInt(total[0]?.n) };
}

export interface NewCustomer {
  name: string;
  kind: CustomerKind;
  contactEmail?: string | null;
  notes?: string;
}

/** Inserts a customer (audited customer.created). */
export async function createCustomer(ctx: ServiceCtx, input: NewCustomer): Promise<CustomerRow> {
  const by = actorColumns(ctx.actor);
  const id = newId('customer');
  const rows = await ctx.tx<CustomerRow[]>`
    insert into customers (id, name, kind, contact_email, notes, created_at, created_by, created_by_label, updated_at)
    values (${id}, ${input.name}, ${input.kind}, ${input.contactEmail ?? null}, ${input.notes ?? ''}, ${ctx.now}, ${by.id}, ${by.label}, ${ctx.now})
    returning *`;
  const row = rows[0]!;
  await auditCtx(ctx, { action: 'customer.created', targetType: 'customer', targetId: id, details: { name: row.name, kind: row.kind, contactEmail: row.contact_email } });
  return row;
}

export interface CustomerUpdate {
  name?: string;
  kind?: CustomerKind;
  contactEmail?: string | null;
  notes?: string;
}

/** Changes the given fields (audited customer.updated with the changed fields; notes only as "changed"). */
export async function updateCustomer(ctx: ServiceCtx, id: string, input: CustomerUpdate): Promise<CustomerRow> {
  const before = await lockCustomer(ctx, id);
  const next = {
    name: input.name ?? before.name,
    kind: input.kind ?? before.kind,
    contact_email: input.contactEmail !== undefined ? input.contactEmail : before.contact_email,
    notes: input.notes ?? before.notes,
  };
  const changes: Record<string, { from: string | null; to: string | null }> = {};
  if (next.name !== before.name) changes.name = { from: before.name, to: next.name };
  if (next.kind !== before.kind) changes.kind = { from: before.kind, to: next.kind };
  if (next.contact_email !== before.contact_email) changes.contactEmail = { from: before.contact_email, to: next.contact_email };
  const notesChanged = next.notes !== before.notes;
  if (Object.keys(changes).length === 0 && !notesChanged) return before;
  const rows = await ctx.tx<CustomerRow[]>`
    update customers set name = ${next.name}, kind = ${next.kind}, contact_email = ${next.contact_email}, notes = ${next.notes}, updated_at = ${ctx.now}
     where id = ${id} returning *`;
  await auditCtx(ctx, { action: 'customer.updated', targetType: 'customer', targetId: id, details: { ...changes, ...(notesChanged ? { notesChanged: true } : {}) } });
  return rows[0]!;
}
