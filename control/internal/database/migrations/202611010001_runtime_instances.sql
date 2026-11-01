CREATE TABLE node_observations (
    node_id UUID PRIMARY KEY REFERENCES nodes(uuid),
    epoch INT8 NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    revision INT8 NOT NULL DEFAULT 0 CHECK (revision >= 0),
    complete_revision INT8 NOT NULL DEFAULT 0 CHECK (complete_revision >= 0 AND complete_revision <= revision),
    observed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE runtime_instances (
    id UUID PRIMARY KEY,
    node_id UUID NOT NULL REFERENCES nodes(uuid),
    service_id UUID NULL,
    deployment_id UUID NULL,
    rollout_id UUID NULL,
    runtime_type STRING NOT NULL CHECK (runtime_type = 'docker'),
    runtime_id STRING NULL,
    name STRING NOT NULL DEFAULT '',
    image STRING NOT NULL DEFAULT '',
    image_id STRING NOT NULL DEFAULT '',
    labels JSONB NOT NULL DEFAULT '{}',
    network_attachments JSONB NOT NULL DEFAULT '{}',
    runtime_state STRING NOT NULL DEFAULT 'unknown',
    health_state STRING NOT NULL DEFAULT '',
    origin STRING NOT NULL CHECK (origin IN ('observed', 'fabric_managed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    observed_at TIMESTAMPTZ NOT NULL,
    removed_at TIMESTAMPTZ NULL,
    epoch INT8 NOT NULL CHECK (epoch >= 0),
    revision INT8 NOT NULL CHECK (revision >= 0),
    CONSTRAINT physical_incarnation UNIQUE (node_id, runtime_type, runtime_id),
    CONSTRAINT observed_runtime_required CHECK (origin = 'fabric_managed' OR runtime_id IS NOT NULL)
);
CREATE INDEX runtime_instances_node_state ON runtime_instances(node_id, runtime_state);
CREATE VIEW runtime_instance_observations AS
SELECT i.*, (n.node_id IS NULL OR n.updated_at < now() - INTERVAL '60 seconds' OR i.epoch < n.epoch) AS observation_stale
FROM runtime_instances AS i LEFT JOIN node_observations AS n ON n.node_id = i.node_id;
