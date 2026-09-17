-- Room-level dialing defaults. Numbers entered without a leading + use this
-- region when a call is dispatched to a sidecar.

ALTER TABLE bridges ADD COLUMN IF NOT EXISTS default_region TEXT NOT NULL DEFAULT '+91';

UPDATE bridges
SET default_region = '+91'
WHERE default_region IS NULL OR default_region = '';

ALTER TABLE bridges DROP CONSTRAINT IF EXISTS bridges_default_region_check;
ALTER TABLE bridges ADD CONSTRAINT bridges_default_region_check
  CHECK (default_region IN ('+1', '+44', '+61', '+65', '+91', '+971'));
