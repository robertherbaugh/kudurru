import type { QueryRow } from '../api/types';

/** Query IDs are local to a node, including in paged and streamed results. */
export const queryIdentity = (row: Pick<QueryRow, 'node' | 'id'>): string => `${encodeURIComponent(row.node)}:${row.id}`;
