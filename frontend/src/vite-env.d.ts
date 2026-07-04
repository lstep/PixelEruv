/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_OTEL_ENABLED?: string;
  readonly VITE_OTEL_ENDPOINT?: string;
  readonly VITE_OTEL_SERVICE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
