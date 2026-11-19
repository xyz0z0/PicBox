-- PicBox Go 服务自建 schema（schema_version = 2）。
-- 只有本文件创建并写入 picbox_meta 标记的数据库才允许打开；
-- 外部（例如 Prisma 生成的）数据库以及旧版（schema_version = 1）数据库
-- 在任何改写型 PRAGMA 之前被拒绝，不做就地迁移。
CREATE TABLE picbox_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE "User" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT UNIQUE NOT NULL,
    username TEXT UNIQUE NOT NULL,
    passwordHash TEXT NOT NULL,
    role TEXT NOT NULL,
    disabledAt TEXT NULL,
    sessionVersion INTEGER NOT NULL DEFAULT 0,
    mustChangePassword INTEGER NOT NULL DEFAULT 0,
    quotaBytes INTEGER NULL,
    lastExportAt TEXT NULL,
    exportReservationAt TEXT NULL,
    exportReservationToken TEXT NULL,
    createdAt TEXT NOT NULL
);

CREATE TABLE "ApiKey" (
    id TEXT PRIMARY KEY,
    userId INTEGER NOT NULL REFERENCES "User"(id),
    name TEXT NOT NULL,
    keyHash TEXT UNIQUE NOT NULL,
    prefix TEXT NOT NULL,
    createdAt TEXT NOT NULL,
    lastUsedAt TEXT NULL,
    revokedAt TEXT NULL
);

CREATE INDEX "ApiKey_userId_idx" ON "ApiKey"(userId);

CREATE TABLE "Image" (
    id TEXT PRIMARY KEY,
    uploaderId INTEGER NOT NULL REFERENCES "User"(id),
    originalName TEXT NOT NULL,
    originalMimeType TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL DEFAULT '',
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    originalPath TEXT NOT NULL,
    thumbPath TEXT NOT NULL,
    createdAt TEXT NOT NULL,
    updatedAt TEXT NOT NULL,
    deletedAt TEXT NULL,
    deletedByAdmin INTEGER NOT NULL DEFAULT 0,
    isVisible INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX "Image_uploaderId_idx" ON "Image"(uploaderId);
CREATE INDEX "Image_createdAt_idx" ON "Image"(createdAt);
CREATE INDEX "Image_deletedAt_idx" ON "Image"(deletedAt);
CREATE INDEX "Image_isVisible_idx" ON "Image"(isVisible);

CREATE TABLE "ImageTag" (
    imageId TEXT NOT NULL REFERENCES "Image"(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    PRIMARY KEY (imageId, name)
);

CREATE INDEX "ImageTag_name_idx" ON "ImageTag"(name);

-- 管理端审计日志。action 只允许原版 14 个动作（同样由 audit.go 校验）。
CREATE TABLE "AuditLog" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actorId INTEGER NOT NULL REFERENCES "User"(id),
    action TEXT NOT NULL CHECK (action IN (
        'ADMIN_LOGIN',
        'ADMIN_USER_CREATE',
        'ADMIN_USER_DISABLE',
        'ADMIN_USER_ENABLE',
        'ADMIN_USER_PASSWORD_RESET',
        'ADMIN_USER_EXPORT_COOLDOWN_RESET',
        'ADMIN_USER_QUOTA_UPDATE',
        'ADMIN_USER_EMAIL_UPDATE',
        'ADMIN_ACTIVATION_CODE_CREATE',
        'ADMIN_ACTIVATION_CODE_REVOKE',
        'ACTIVATION_CODE_USED',
        'ADMIN_IMAGE_HIDE',
        'ADMIN_IMAGE_SHOW',
        'ADMIN_IMAGE_DELETE'
    )),
    targetUserId INTEGER NULL REFERENCES "User"(id),
    detail TEXT NULL,
    createdAt TEXT NOT NULL
);

CREATE INDEX "AuditLog_createdAt_idx" ON "AuditLog"(createdAt);
CREATE INDEX "AuditLog_action_idx" ON "AuditLog"(action);

-- 注册激活码：只保存高熵激活码的 sha256 哈希，完整码只在生成响应里出现一次。
CREATE TABLE "ActivationCode" (
    id TEXT PRIMARY KEY,
    codeHash TEXT UNIQUE NOT NULL,
    maskedCode TEXT NOT NULL,
    note TEXT NULL,
    createdById INTEGER NOT NULL REFERENCES "User"(id),
    usedById INTEGER NULL REFERENCES "User"(id),
    createdAt TEXT NOT NULL,
    expiresAt TEXT NULL,
    usedAt TEXT NULL,
    revokedAt TEXT NULL
);

CREATE INDEX "ActivationCode_createdAt_idx" ON "ActivationCode"(createdAt);
CREATE INDEX "ActivationCode_codeHash_idx" ON "ActivationCode"(codeHash);

-- 找回密码 / 修改邮箱的一次性令牌。只落库 hash；同一账号每种用途至多一条。
CREATE TABLE "AccountToken" (
    id TEXT PRIMARY KEY,
    userId INTEGER NOT NULL REFERENCES "User"(id),
    purpose TEXT NOT NULL CHECK (purpose IN ('PASSWORD_RESET', 'EMAIL_CHANGE')),
    tokenHash TEXT UNIQUE NOT NULL,
    email TEXT NOT NULL,
    currentEmail TEXT NOT NULL,
    sessionVersion INTEGER NOT NULL,
    expiresAt TEXT NOT NULL,
    createdAt TEXT NOT NULL,
    UNIQUE (userId, purpose)
);

CREATE INDEX "AccountToken_tokenHash_idx" ON "AccountToken"(tokenHash);
