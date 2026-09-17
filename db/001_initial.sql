CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS operators (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bridges (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS participants (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  display_name TEXT NOT NULL,
  phone_number TEXT NOT NULL,
  desired_state TEXT NOT NULL DEFAULT 'ACTIVE',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS call_legs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  participant_id UUID NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  node_id TEXT,
  node_epoch BIGINT,
  sidecar_instance_id TEXT,
  pbx_call_id TEXT,
  sip_call_id TEXT,
  sip_from_tag TEXT,
  sip_to_tag TEXT,
  pbx_member_id TEXT,
  state TEXT NOT NULL DEFAULT 'REQUESTED',
  desired_mute BOOLEAN NOT NULL DEFAULT false,
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  answered_at TIMESTAMPTZ,
  ended_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_call_legs_active ON call_legs(participant_id, state);
CREATE INDEX IF NOT EXISTS idx_call_legs_owner ON call_legs(node_id, state);

CREATE TABLE IF NOT EXISTS dial_batches (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  bridge_id UUID NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'QUEUED',
  concurrency INTEGER NOT NULL DEFAULT 16,
  rate_per_second INTEGER NOT NULL DEFAULT 8,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dial_batch_items (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  batch_id UUID NOT NULL REFERENCES dial_batches(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  phone_number TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'QUEUED',
  participant_id UUID REFERENCES participants(id) ON DELETE SET NULL,
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS node_commands (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  command_id TEXT UNIQUE NOT NULL,
  node_id TEXT NOT NULL,
  node_epoch BIGINT NOT NULL,
  call_leg_id UUID REFERENCES call_legs(id) ON DELETE SET NULL,
  action TEXT NOT NULL,
  request JSONB NOT NULL,
  result JSONB,
  status TEXT NOT NULL DEFAULT 'PENDING',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS audit_events (
  id BIGSERIAL PRIMARY KEY,
  actor TEXT NOT NULL,
  event_type TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
