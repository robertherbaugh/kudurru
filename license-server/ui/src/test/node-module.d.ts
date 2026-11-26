// The one Node API the UI tests use (queryClients.ts finds the scheduler
// module react-dom loads). The UI program has no Node types, so this
// declares only that.
declare module 'node:module' {
  interface Require {
    (id: string): unknown;
    resolve(id: string): string;
  }
  export function createRequire(path: string | URL): Require;
}
