export const MAX_IMAGE_TAG_LENGTH = 64;
export const MAX_IMAGE_TAGS = 20;
export const IMAGE_TAG_ERROR = "每张图片最多 20 个标签，每个最多 64 个字，且不能包含控制字符";

export type TagMode = "add" | "remove" | "replace";

export function parseImageTag(value: unknown): string | null | undefined {
  if (typeof value !== "string") return undefined;
  if (/[\u0000-\u001f\u007f]/.test(value)) return undefined;
  const tag = value.trim();
  if (!tag) return null;
  if (Array.from(tag).length > MAX_IMAGE_TAG_LENGTH) {
    return undefined;
  }
  return tag;
}

export function parseImageTags(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const tags = new Set<string>();
  for (const entry of value) {
    const tag = parseImageTag(entry);
    if (tag === undefined) return undefined;
    if (tag !== null) tags.add(tag);
    if (tags.size > MAX_IMAGE_TAGS) return undefined;
  }
  return [...tags].sort();
}

/** New arrays and the legacy scalar must never be combined implicitly. */
export function parseTagFields(fields: { tag?: unknown; tags?: unknown }): string[] | undefined {
  if (fields.tag !== undefined && fields.tags !== undefined) return undefined;
  if (fields.tags !== undefined) return parseImageTags(fields.tags);
  if (fields.tag !== undefined) {
    const tag = parseImageTag(fields.tag);
    return tag === undefined ? undefined : tag === null ? [] : [tag];
  }
  return [];
}

export function mergeImageTags(current: string[], incoming: string[], mode: TagMode): string[] | undefined {
  return parseImageTags(mode === "replace" ? incoming : mode === "add"
    ? [...current, ...incoming]
    : current.filter((tag) => !incoming.includes(tag)));
}

export function imageTagUpdate(tags: string[]) {
  return {
    tags: { deleteMany: {}, create: tags.map((name) => ({ name })) },
  };
}
