-- +goose Up
-- Yakuniy sxema: docs/GO_BACKEND_HANDOFF.md 4-bo'lim. Nomlar/turlar Supabase bilan bir xil
-- (pg_dump --data-only bilan ko'chirish uchun). auth.users o'rniga auth_credentials.

create extension if not exists pgcrypto;

create table app_settings (
    id                  boolean  primary key default true check (id),
    default_max_devices smallint not null default 1,
    telegram_username   text,
    support_phone       text,
    sms_text            text,
    price_basic text, price_basic_3m text, price_basic_6m text, price_basic_12m text,
    price_plus  text, price_plus_3m  text, price_plus_6m  text, price_plus_12m  text,
    price_pro   text, price_pro_3m   text, price_pro_6m   text, price_pro_12m   text,
    free_max_debtors int,  free_max_debts int,
    basic_max_debtors int, basic_max_debts int,
    plus_max_debtors int,  plus_max_debts int,
    pro_max_debtors int,   pro_max_debts int
);
insert into app_settings (id) values (true);

create table premium_users (
    id                      uuid        primary key,
    login                   text        not null unique,
    full_name               text        not null,
    description             text        not null default '',
    phone_number            text        not null default '',
    premium_plan_type_index smallint    not null default 0,
    start_at                timestamptz not null default now(),
    expiry_at               timestamptz not null,
    is_active               boolean     not null default true,
    created_at              timestamptz not null default now(),
    max_devices             smallint,
    credentials_updated_at  timestamptz not null default now(),
    signup_source           text        not null default 'ADMIN' check (signup_source in ('ADMIN','SELF','GOOGLE')),
    google_sub              text        unique
);

create table auth_credentials (
    user_id       uuid primary key references premium_users(id) on delete cascade,
    password_hash text not null,
    role          text not null default 'user' check (role in ('user','admin'))
);

create table refresh_tokens (
    id          uuid primary key,
    user_id     uuid not null references premium_users(id) on delete cascade,
    token_hash  text not null unique,
    device_id   text,
    created_at  timestamptz not null default now(),
    expires_at  timestamptz not null,
    revoked_at  timestamptz,
    -- true => revoked by rotation (not logout/admin). Such a token is accepted once more for
    -- 60 s so a lost /refresh response on a flaky mobile network doesn't log the user out.
    rotated     boolean not null default false
);
create index on refresh_tokens (user_id);

create table user_devices (
    premium_user_id uuid not null references premium_users(id) on delete cascade,
    device_id       text not null,
    device_name     text not null default '',
    last_seen_at    timestamptz not null default now(),
    created_at      timestamptz not null default now(),
    primary key (premium_user_id, device_id)
);

-- Global monoton sync soati (3.3). Har PUSH tranzaksiyasi boshida shu qatorni qulflab
-- greatest(now_ms, last_ms + 1) oladi va commit'gacha ushlaydi => last_updated commit tartibida.
create table sync_clock (
    id      boolean primary key default true check (id),
    last_ms bigint  not null
);
insert into sync_clock (id, last_ms) values (true, 0);

create table debtors (
    id                 uuid primary key,
    premium_user_id    uuid not null references premium_users(id) on delete cascade,
    full_name          text not null,
    description        text not null default '',
    phone_number       text not null default '',
    currency_type      smallint not null default 0,
    total_debts_amount bigint not null default 0,
    repayment_date     timestamptz not null,
    is_deleted         boolean not null default false,
    last_updated       bigint not null,
    updated_at_client  bigint
);
create index on debtors (premium_user_id, last_updated, id);

create table debts (
    id               uuid primary key,
    debtor_id        uuid not null references debtors(id) on delete cascade,
    debt_amount      bigint not null,
    debt_description text not null default '',
    debt_date        timestamptz not null,
    is_deleted       boolean not null default false,
    last_updated     bigint not null
);
create index on debts (debtor_id);
create index on debts (last_updated, id);

create table finance_categories (
    id                text not null,
    premium_user_id   uuid not null references premium_users(id) on delete cascade,
    label             text not null,
    type              text not null,
    color_key         text not null,
    icon_key          text not null,
    is_hidden         boolean not null default false,
    is_deleted        boolean not null default false,
    last_updated      bigint not null,
    updated_at_client bigint not null default 0,
    primary key (premium_user_id, id)
);
create index on finance_categories (premium_user_id, last_updated, id);

create table finance_tags (
    id                uuid primary key,
    premium_user_id   uuid not null references premium_users(id) on delete cascade,
    name              text not null,
    color_key         text not null,
    is_deleted        boolean not null default false,
    last_updated      bigint not null,
    updated_at_client bigint not null default 0
);
create index on finance_tags (premium_user_id, last_updated, id);

create table finance_goals (
    id                uuid primary key,
    premium_user_id   uuid not null references premium_users(id) on delete cascade,
    name              text not null,
    note              text not null default '',
    currency          text not null,
    target_amount     bigint not null,
    saved_amount      bigint not null default 0,
    color_key         text not null,
    icon_key          text not null,
    deadline          text,
    is_deleted        boolean not null default false,
    last_updated      bigint not null,
    updated_at_client bigint not null default 0
);
create index on finance_goals (premium_user_id, last_updated, id);

create table finance_transactions (
    id                 uuid primary key,
    premium_user_id    uuid not null references premium_users(id) on delete cascade,
    type               text not null,
    currency           text not null,
    amount             bigint not null,
    to_currency        text,
    to_amount          bigint,
    rate               text,
    category_id        text not null default '',
    description        text not null default '',
    tag_ids            text not null default '',
    tx_date            text not null,
    linked_debt_id     uuid,
    is_opening_balance boolean not null default false,
    is_deleted         boolean not null default false,
    last_updated       bigint not null,
    updated_at_client  bigint not null default 0
);
create index on finance_transactions (premium_user_id, last_updated, id);

create table notes (
    id                uuid primary key,
    premium_user_id   uuid not null references premium_users(id) on delete cascade,
    title             text not null default '',
    content           text not null default '',
    color_key         text not null default 'DEFAULT',
    labels            text not null default '',
    is_pinned         boolean not null default false,
    is_archived       boolean not null default false,
    reminder_at       bigint,
    is_deleted        boolean not null default false,
    last_updated      bigint not null default 0,
    updated_at_client bigint not null default 0
);
create index on notes (premium_user_id, last_updated, id);

create table announcements (
    id              uuid primary key default gen_random_uuid(),
    title           text not null,
    body            text not null,
    target          text not null default 'ALL' check (target in ('ALL','PLAN','USER')),
    target_plan     smallint,
    target_user_id  uuid references premium_users(id) on delete cascade,
    is_deleted      boolean not null default false,
    created_at      timestamptz not null default now()
);
create index on announcements (created_at desc);

create table announcement_reads (
    premium_user_id uuid not null references premium_users(id) on delete cascade,
    announcement_id uuid not null references announcements(id) on delete cascade,
    is_hidden       boolean not null default false,
    read_at         timestamptz not null default now(),
    primary key (premium_user_id, announcement_id)
);

create table payments (
    id               uuid primary key default gen_random_uuid(),
    premium_user_id  uuid not null references premium_users(id) on delete cascade,
    amount           bigint not null check (amount >= 0),
    currency         text not null default 'UZS',
    plan_type_index  smallint not null,
    months           smallint not null check (months > 0),
    method           text not null,
    note             text not null default '',
    paid_at          timestamptz not null default now(),
    created_at       timestamptz not null default now()
);
create index on payments (premium_user_id, paid_at desc);

create table admin_audit_log (
    id              uuid primary key default gen_random_uuid(),
    action          text not null,
    target_user_id  uuid,
    target_label    text not null default '',
    details         text not null default '',
    created_at      timestamptz not null default now()
);
create index on admin_audit_log (created_at desc);

create table account_deletion_requests (
    id               uuid primary key default gen_random_uuid(),
    premium_user_id  uuid not null,
    login            text not null,
    contact_phone    text not null,
    account_phone    text not null default '',
    lang             text not null default 'uz',
    status           text not null default 'PENDING' check (status in ('PENDING','APPROVED','REJECTED')),
    admin_note       text not null default '',
    ip               text not null default '',
    user_agent       text not null default '',
    created_at       timestamptz not null default now(),
    resolved_at      timestamptz
);
create unique index on account_deletion_requests (premium_user_id) where status = 'PENDING';
create index on account_deletion_requests (status, created_at desc);

-- Google Play Billing (8-bo'lim, 2-bosqich)
create table purchases (
    purchase_token        text primary key,
    premium_user_id       uuid not null references premium_users(id) on delete cascade,
    product_id            text not null,
    base_plan_id          text,
    order_id              text,
    state                 text not null,
    expiry_at             timestamptz,
    linked_purchase_token text,
    raw                   jsonb,
    updated_at            timestamptz not null default now()
);

-- +goose Down
drop table if exists purchases, account_deletion_requests, admin_audit_log, payments,
    announcement_reads, announcements, notes, finance_transactions, finance_goals, finance_tags,
    finance_categories, debts, debtors, sync_clock, user_devices, refresh_tokens, auth_credentials,
    premium_users, app_settings cascade;
