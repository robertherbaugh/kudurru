/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** True in `vite --mode mock` (MSW answers /api/v1 in the browser). */
  readonly KUDURRU_MOCK: boolean;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
