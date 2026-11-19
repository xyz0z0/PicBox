declare const __PICBOX_VERSION__: string;

/**
 * The reused DashboardClient reads process.env.NEXT_PUBLIC_UPLOAD_MAX_BYTES.
 * Vite replaces that exact expression at build time (see vite.config.ts), so the
 * browser never evaluates `process`; this declaration only keeps type checking
 * of the original component honest.
 */
declare const process: {
  env: {
    NEXT_PUBLIC_UPLOAD_MAX_BYTES?: string;
  };
};
