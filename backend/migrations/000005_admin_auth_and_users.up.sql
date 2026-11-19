CREATE TABLE admin_users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'ADMIN',
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (username ~ '^[a-zA-Z][a-zA-Z0-9_.-]{2,63}$'),
    CHECK (length(password_hash) >= 20),
    CHECK (length(btrim(display_name)) > 0),
    CHECK (role IN ('ADMIN'))
);

ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'ACTIVE';
ALTER TABLE users ADD COLUMN note TEXT;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'DISABLED'));

COMMENT ON TABLE users IS '平台托管用户表';
COMMENT ON COLUMN users.status IS '用户状态：ACTIVE 或 DISABLED';
COMMENT ON COLUMN users.note IS '管理员维护的用户备注';

COMMENT ON TABLE admin_users IS '管理后台管理员账户表';
COMMENT ON COLUMN admin_users.id IS '管理员自增主键';
COMMENT ON COLUMN admin_users.username IS '管理员登录用户名';
COMMENT ON COLUMN admin_users.password_hash IS '管理员密码哈希';
COMMENT ON COLUMN admin_users.display_name IS '管理员展示名称';
COMMENT ON COLUMN admin_users.role IS '管理员角色，当前固定为 ADMIN';
COMMENT ON COLUMN admin_users.disabled IS '是否禁用登录';
COMMENT ON COLUMN admin_users.last_login_at IS '最近一次成功登录时间';
COMMENT ON COLUMN admin_users.created_at IS '管理员创建时间';
COMMENT ON COLUMN admin_users.updated_at IS '管理员最后更新时间';
