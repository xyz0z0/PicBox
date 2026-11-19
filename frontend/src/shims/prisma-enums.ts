/**
 * Mirrors the values of the generated Prisma enum used by the original client
 * components. The generated Prisma client is not part of this repository, so
 * the browser bundle must not depend on it; it only needs the two role strings.
 */
export const UserRole = {
  USER: "USER",
  ADMIN: "ADMIN",
} as const;

export type UserRole = (typeof UserRole)[keyof typeof UserRole];
