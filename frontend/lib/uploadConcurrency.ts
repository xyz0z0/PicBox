/**
 * Number of images the upload dialog sends at the same time.
 *
 * The PicBox server deliberately allows a single upload slot and answers
 * 429 UPLOAD_BUSY when that slot is taken, so a multi-way client would turn a
 * queued upload into a burst of rate-limit errors. The dialog keeps its queue,
 * progress and retry UI, but drains the queue one image at a time.
 */
export const DEFAULT_UPLOAD_CONCURRENCY = 1;
