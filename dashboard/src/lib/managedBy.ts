// Ownership of objects (D9, §4.3): the kudurru.io/managed-by annotation
// names the single writer. The UI edits only what the API may write and
// shows everything else read-only with its owner (§10 principle 6).
import type { AnyObject } from '../api/types';

export const MANAGED_BY = 'kudurru.io/managed-by';
/** Free-text comment kept as an annotation on kinds without a comment field. */
export const COMMENT = 'kudurru.io/comment';
/** "false" disables an object in the UI; see dev-docs/milestones/m3/ui.md (contract gap). */
export const ENABLED = 'kudurru.io/enabled';

export type OwnerKind = 'api' | 'bootstrap' | 'file' | 'kubernetes' | 'integration' | 'system' | 'none';

export interface Ownership {
  /** The raw annotation value ("" when absent). */
  owner: string;
  kind: OwnerKind;
  /** Short badge text. */
  label: string;
  /** Whether a write through the API is expected to succeed. */
  editable: boolean;
}

/** Classifies a managed-by value. */
export function ownership(owner: string | undefined): Ownership {
  const o = (owner ?? '').trim();
  if (o === '') return { owner: o, kind: 'none', label: 'unmanaged', editable: true };
  if (o === 'api') return { owner: o, kind: 'api', label: 'API', editable: true };
  if (o === 'bootstrap') return { owner: o, kind: 'bootstrap', label: 'default', editable: true };
  if (o === 'system') return { owner: o, kind: 'system', label: 'system', editable: false };
  if (o.startsWith('file:')) return { owner: o, kind: 'file', label: 'file', editable: false };
  if (o.startsWith('integration:')) return { owner: o, kind: 'integration', label: o.slice('integration:'.length), editable: false };
  if (o === 'kubernetes' || o === 'kubectl' || o.startsWith('kubernetes:') || o.startsWith('kube:')) {
    return { owner: o, kind: 'kubernetes', label: 'Kubernetes', editable: false };
  }
  return { owner: o, kind: 'system', label: o, editable: false };
}

export function objectOwnership(obj: Pick<AnyObject, 'metadata'>): Ownership {
  return ownership(obj.metadata.annotations?.[MANAGED_BY]);
}

/** A sentence explaining why an object is read-only. */
export function readOnlyReason(o: Ownership): string {
  switch (o.kind) {
    case 'file':
      return `Managed by ${o.owner.slice('file:'.length)}; edit the file on the host.`;
    case 'kubernetes':
      return 'Managed in Kubernetes; change it with kubectl or GitOps.';
    case 'integration':
      return `Managed by the ${o.label} integration.`;
    case 'system':
      return `Managed by ${o.owner}.`;
    default:
      return '';
  }
}

export function objectComment(obj: Pick<AnyObject, 'metadata'>): string {
  return obj.metadata.annotations?.[COMMENT] ?? '';
}

export function objectEnabled(obj: Pick<AnyObject, 'metadata'>): boolean {
  return obj.metadata.annotations?.[ENABLED] !== 'false';
}

/** Returns a copy with the comment and enabled annotations set. */
export function withMeta<T extends AnyObject>(obj: T, comment: string, enabled: boolean): T {
  const annotations = { ...(obj.metadata.annotations ?? {}) };
  if (comment.trim()) annotations[COMMENT] = comment.trim();
  else delete annotations[COMMENT];
  if (enabled) delete annotations[ENABLED];
  else annotations[ENABLED] = 'false';
  return { ...obj, metadata: { ...obj.metadata, annotations } };
}
