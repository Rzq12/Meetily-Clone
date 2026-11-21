CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      VARCHAR(50)  NOT NULL UNIQUE,
    full_name     VARCHAR(255) NOT NULL,
    password_hash TEXT         NOT NULL,
    role          VARCHAR(20)  NOT NULL CHECK (role IN ('admin', 'notetaker', 'viewer')),
    is_active     BOOLEAN      NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash   CHAR(64)    PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(100) NOT NULL UNIQUE,
    description VARCHAR(500),
    sections    JSONB        NOT NULL,
    is_default  BOOLEAN      NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX templates_one_default ON templates (is_default) WHERE is_default;

CREATE TABLE meetings (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title              VARCHAR(255) NOT NULL,
    meeting_date       DATE         NOT NULL,
    owner_id           UUID         NOT NULL REFERENCES users(id),
    template_id        UUID         NOT NULL REFERENCES templates(id),
    source             VARCHAR(10)  NOT NULL CHECK (source IN ('upload', 'browser')),
    status             VARCHAR(20)  NOT NULL CHECK (status IN ('recording', 'queued', 'transcribing', 'summarizing', 'done', 'failed')),
    audio_path         TEXT         NOT NULL,
    audio_duration_sec INTEGER,
    last_chunk_seq     INTEGER      NOT NULL DEFAULT 0,
    skip_transcription BOOLEAN      NOT NULL DEFAULT false,
    error_message      TEXT,
    minutes_markdown   TEXT,
    minutes_backup     TEXT,
    minutes_edited_by  UUID REFERENCES users(id),
    minutes_edited_at  TIMESTAMPTZ,
    queued_at          TIMESTAMPTZ,
    audio_deleted_at   TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX meetings_status_queued_at ON meetings (status, queued_at);
CREATE INDEX meetings_owner_id ON meetings (owner_id);
CREATE INDEX meetings_meeting_date ON meetings (meeting_date);

CREATE TABLE meeting_participants (
    meeting_id UUID        NOT NULL REFERENCES meetings(id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, user_id)
);

CREATE INDEX meeting_participants_user_id ON meeting_participants (user_id);

CREATE TABLE transcript_segments (
    id         BIGSERIAL PRIMARY KEY,
    meeting_id UUID    NOT NULL REFERENCES meetings(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    start_sec  REAL    NOT NULL,
    end_sec    REAL    NOT NULL,
    text       TEXT    NOT NULL,
    UNIQUE (meeting_id, seq)
);

CREATE TABLE audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    user_id    UUID REFERENCES users(id),
    action     VARCHAR(40) NOT NULL,
    meeting_id UUID,
    ip_address VARCHAR(45) NOT NULL,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_created_at ON audit_logs (created_at);
CREATE INDEX audit_logs_user_id ON audit_logs (user_id);
