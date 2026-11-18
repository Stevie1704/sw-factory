-- Store baseline schema 53. Keep the schema version in Go, not in this DDL.
-- Child identities are enforced by the store; tables intentionally have no foreign keys.

CREATE TABLE evaluation_dispositions (
    run_id TEXT NOT NULL,
    event_id_hash TEXT NOT NULL,
    category TEXT NOT NULL,
    disposition TEXT NOT NULL,
    decided_at TEXT NOT NULL,
    PRIMARY KEY (run_id, event_id_hash)
);

CREATE TABLE evaluation_summaries (
    run_id TEXT PRIMARY KEY,
    outcome TEXT NOT NULL,
    started_at TEXT NOT NULL,
    completed_at TEXT NOT NULL DEFAULT '',
    total_wall_nanos INTEGER NOT NULL DEFAULT 0,
    stage_durations TEXT NOT NULL DEFAULT '[]',
    invocation_versions TEXT NOT NULL DEFAULT '[]',
    invocation_count INTEGER NOT NULL DEFAULT 0,
    gate_count INTEGER NOT NULL DEFAULT 0,
    check_repair_count INTEGER NOT NULL DEFAULT 0,
    test_revision_count INTEGER NOT NULL DEFAULT 0,
    review_revision_count INTEGER NOT NULL DEFAULT 0,
    budget_exhausted INTEGER NOT NULL DEFAULT 0,
    exemptions TEXT NOT NULL DEFAULT '[]',
    escalation_categories TEXT NOT NULL DEFAULT '[]',
    repeated_blockers TEXT NOT NULL DEFAULT '[]',
    usage_available INTEGER NOT NULL DEFAULT 0,
    usage_unavailable_reason TEXT NOT NULL DEFAULT 'not_reported',
    usage_input_tokens INTEGER NOT NULL DEFAULT 0,
    usage_output_tokens INTEGER NOT NULL DEFAULT 0,
    usage_total_tokens INTEGER NOT NULL DEFAULT 0,
    usage_cost_reported INTEGER NOT NULL DEFAULT 0,
    usage_cost_micros INTEGER NOT NULL DEFAULT 0,
    usage_currency TEXT NOT NULL DEFAULT '',
    active_stage TEXT NOT NULL DEFAULT '',
    active_stage_started_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    review_diff_bytes INTEGER NOT NULL DEFAULT 0,
    review_changed_lines INTEGER NOT NULL DEFAULT 0,
    review_unit_count INTEGER NOT NULL DEFAULT 0,
    review_largest_unit_bytes INTEGER NOT NULL DEFAULT 0,
    review_invocation_count INTEGER NOT NULL DEFAULT 0,
    review_duration_nanos INTEGER NOT NULL DEFAULT 0,
    review_finding_counts TEXT NOT NULL DEFAULT '{}',
    review_incomplete_unit_count INTEGER NOT NULL DEFAULT 0,
    review_cannot_proceed_unit_count INTEGER NOT NULL DEFAULT 0,
    review_over_budget_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE evaluation_usage (
    run_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_reported INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (run_id, invocation_id)
);

CREATE TABLE gate_results (
    run_id TEXT NOT NULL,
    checkpoint_sha TEXT NOT NULL,
    phase TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    gate_name TEXT NOT NULL,
    outcome TEXT NOT NULL,
    status TEXT NOT NULL,
    blocking INTEGER NOT NULL,
    skip_reason TEXT NOT NULL DEFAULT '',
    setup_fingerprint TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, checkpoint_sha, phase, gate_name)
);

CREATE TABLE ideation_approvals (
    session_id TEXT NOT NULL,
    id TEXT NOT NULL,
    plan_revision INTEGER NOT NULL,
    target TEXT NOT NULL,
    payload BLOB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    relationship_mapping BLOB NOT NULL,
    relationship_mapping_sha256 TEXT NOT NULL,
    approved_at TEXT NOT NULL,
    PRIMARY KEY (session_id, id)
);

CREATE TABLE ideation_brief_revisions (
    session_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    brief TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (session_id, revision)
);

CREATE TABLE ideation_commands (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    expected_revision INTEGER NOT NULL,
    payload TEXT NOT NULL,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    outcome TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    session_revision INTEGER NOT NULL DEFAULT 0,
    result TEXT NOT NULL DEFAULT '',
    accepted_at TEXT NOT NULL,
    completed_at TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT 'session',
    subject_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE ideation_invocations (
    session_id TEXT NOT NULL,
    id TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    trigger_id TEXT NOT NULL,
    session_revision INTEGER NOT NULL,
    brief_revision INTEGER NOT NULL DEFAULT 0,
    context_revision TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    prompt_version TEXT NOT NULL,
    prompt_sha256 TEXT NOT NULL,
    image TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    credential_store_id TEXT NOT NULL DEFAULT '',
    worker_id TEXT NOT NULL,
    native_session_id TEXT NOT NULL DEFAULT '',
    resumed_from TEXT NOT NULL DEFAULT '',
    snapshot_path TEXT NOT NULL DEFAULT '',
    invocation_directory TEXT NOT NULL DEFAULT '',
    result_directory TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    failure_kind TEXT NOT NULL DEFAULT '',
    status_reason TEXT NOT NULL DEFAULT '',
    payload BLOB,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    resources_released_at TEXT
);

CREATE TABLE ideation_plan_revisions (
    session_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    source_brief_revision INTEGER NOT NULL,
    context_revision TEXT NOT NULL,
    nodes TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (session_id, revision)
);

CREATE TABLE ideation_publication_operations (
    session_id TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    kind TEXT NOT NULL,
    node_id TEXT NOT NULL DEFAULT '',
    correlation TEXT NOT NULL DEFAULT '',
    relationship_kind TEXT NOT NULL DEFAULT '',
    from_node_id TEXT NOT NULL DEFAULT '',
    to_node_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (session_id, approval_id, id),
    UNIQUE (session_id, approval_id, sequence)
);

CREATE TABLE ideation_publication_receipts (
    session_id TEXT NOT NULL,
    id TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    items TEXT NOT NULL,
    relationships TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (session_id, id)
);

CREATE TABLE ideation_sessions (
    id TEXT PRIMARY KEY,
    repository_path TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL,
    brief_revision INTEGER NOT NULL DEFAULT 0,
    plan_revision INTEGER NOT NULL DEFAULT 0,
    approval_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE ideation_turns (
    session_id TEXT NOT NULL,
    id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    author TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT '',
    invocation_id TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    session_revision INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    corrects TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, id),
    UNIQUE (session_id, sequence)
);

CREATE TABLE "invocations" (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    harness TEXT NOT NULL,
    role TEXT NOT NULL,
    stage TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    native_session_id TEXT NOT NULL DEFAULT '',
    invocation_directory TEXT NOT NULL DEFAULT '',
    result_directory TEXT NOT NULL DEFAULT '',
    prompt_version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    permitted_paths TEXT NOT NULL DEFAULT '',
    credential_store_id TEXT NOT NULL DEFAULT '',
    recovery_resume_count INTEGER NOT NULL DEFAULT 0,
    manual_resume_count INTEGER NOT NULL DEFAULT 0,
    prompt_craft_source_path TEXT NOT NULL DEFAULT '',
    prompt_craft_sha256 TEXT NOT NULL DEFAULT '',
    launch_voided INTEGER NOT NULL DEFAULT 0,
    review_round_id TEXT NOT NULL DEFAULT '',
    review_unit_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE local_backlog (
    repository_path TEXT PRIMARY KEY,
    projection BLOB NOT NULL
);

CREATE TABLE local_claims (
    claim_key TEXT PRIMARY KEY,
    repository_path TEXT NOT NULL,
    run_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL,
    claim BLOB NOT NULL,
    terminal_status TEXT NOT NULL DEFAULT '',
    merge_sha TEXT NOT NULL DEFAULT ''
);

CREATE TABLE local_execution_authorizations (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL UNIQUE,
    preview_id TEXT NOT NULL,
    repository_path TEXT NOT NULL,
    binding BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE local_execution_invalidations (
    request_id TEXT PRIMARY KEY,
    repository_path TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_at TEXT NOT NULL
);

CREATE TABLE local_execution_previews (
    revision INTEGER PRIMARY KEY AUTOINCREMENT,
    preview_id TEXT NOT NULL UNIQUE,
    repository_path TEXT NOT NULL,
    binding BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE local_issue_comments (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_path TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    author TEXT NOT NULL,
    body TEXT NOT NULL,
    creation_key TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(repository_path,issue_id,creation_key)
);

CREATE TABLE local_issue_identities (
    number INTEGER PRIMARY KEY AUTOINCREMENT CHECK(number > 0 AND number <= 2147483647),
    repository_path TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    correlation TEXT NOT NULL,
    UNIQUE(repository_path, issue_id),
    UNIQUE(repository_path, correlation)
);

CREATE TABLE local_issue_projections (
    repository_path TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'open',
    labels TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY(repository_path,issue_id)
);

CREATE TABLE local_issue_snapshots (
    repository_path TEXT NOT NULL,
    revision TEXT NOT NULL,
    graph_digest TEXT NOT NULL,
    PRIMARY KEY(repository_path,revision)
);

CREATE TABLE local_issue_sources (
    repository_path TEXT NOT NULL,
    revision TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    source_path TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    content BLOB NOT NULL,
    PRIMARY KEY(repository_path,revision,issue_id)
);

CREATE TABLE operational_runs (
    id TEXT PRIMARY KEY,
    repository_path TEXT NOT NULL,
    issue_number INTEGER NOT NULL,
    stage TEXT NOT NULL,
    status TEXT NOT NULL,
    branch TEXT NOT NULL DEFAULT '',
    worktree TEXT NOT NULL DEFAULT '',
    checkpoint_sha TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    coordinator TEXT NOT NULL DEFAULT '',
    status_comment_id TEXT NOT NULL DEFAULT '',
    specification_packet TEXT NOT NULL DEFAULT '',
    pull_request_number INTEGER NOT NULL DEFAULT 0,
    pull_request_url TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 0,
    processed_comment_id TEXT NOT NULL DEFAULT '',
    processed_comment_revision INTEGER NOT NULL DEFAULT 0,
    last_command_name TEXT NOT NULL DEFAULT '',
    last_command_outcome TEXT NOT NULL DEFAULT '',
    last_command_message TEXT NOT NULL DEFAULT '',
    harness_override TEXT NOT NULL DEFAULT '',
    merge_commit_sha TEXT NOT NULL DEFAULT '',
    lifecycle_reason TEXT NOT NULL DEFAULT '',
    check_repair_attempts INTEGER NOT NULL DEFAULT 0,
    check_repair_budget INTEGER NOT NULL DEFAULT 0,
    check_repair_pending_attempt INTEGER NOT NULL DEFAULT 0,
    pending_questions TEXT NOT NULL DEFAULT '[]',
    clarification_comment_id TEXT NOT NULL DEFAULT '',
    base_checkpoint_sha TEXT NOT NULL DEFAULT '',
    test_checkpoint_sha TEXT NOT NULL DEFAULT '',
    test_handoff TEXT NOT NULL DEFAULT '',
    test_exemption TEXT NOT NULL DEFAULT '',
    protected_test_paths TEXT NOT NULL DEFAULT '[]',
    test_stage_skipped INTEGER NOT NULL DEFAULT 0,
    implementation_handoff TEXT NOT NULL DEFAULT '',
    specification_review TEXT NOT NULL DEFAULT '',
    terminal_at TEXT NOT NULL DEFAULT '',
    test_invocation_id TEXT NOT NULL DEFAULT '',
    test_revision_attempts INTEGER NOT NULL DEFAULT 0,
    test_revision_budget INTEGER NOT NULL DEFAULT 0,
    test_revision_history TEXT NOT NULL DEFAULT '[]',
    test_objection TEXT NOT NULL DEFAULT '',
    test_revision_base_changed_paths TEXT NOT NULL DEFAULT '[]',
    standards_review TEXT NOT NULL DEFAULT '',
    active_invocation_ids TEXT NOT NULL DEFAULT '[]',
    review_repair_attempts INTEGER NOT NULL DEFAULT 0,
    review_repair_budget INTEGER NOT NULL DEFAULT 0,
    review_repair_pending_attempt INTEGER NOT NULL DEFAULT 0,
    review_repair_history TEXT NOT NULL DEFAULT '[]',
    review_repair_packet TEXT NOT NULL DEFAULT '',
    processed_review_id TEXT NOT NULL DEFAULT '',
    processed_review_revision INTEGER NOT NULL DEFAULT 0,
    accepted_implementation_checkpoint_sha TEXT NOT NULL DEFAULT '',
    pull_request_summary TEXT NOT NULL DEFAULT '',
    tracker_repository TEXT NOT NULL DEFAULT ''
);

CREATE TABLE pending_effects (
    run_id TEXT PRIMARY KEY,
    effect_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE review_rounds (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    base_checkpoint_sha TEXT NOT NULL,
    checkpoint_sha TEXT NOT NULL,
    diff_path TEXT NOT NULL,
    diff_bytes INTEGER NOT NULL,
    diff_sha256 TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    schema_version INTEGER NOT NULL,
    policy_version TEXT NOT NULL,
    max_unit_bytes INTEGER NOT NULL,
    max_units INTEGER NOT NULL,
    context_lines INTEGER NOT NULL,
    concurrency INTEGER NOT NULL DEFAULT 2,
    authorized_max_units INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE review_unit_results (
    round_id TEXT NOT NULL,
    role TEXT NOT NULL,
    unit_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    checkpoint_sha TEXT NOT NULL,
    outcome TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    questions TEXT NOT NULL,
    evidence TEXT NOT NULL,
    findings TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (round_id, role, unit_id)
);

CREATE TABLE review_units (
    round_id TEXT NOT NULL,
    unit_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    workload_bytes INTEGER NOT NULL,
    diff_sha256 TEXT NOT NULL,
    segments TEXT NOT NULL,
    primary_ranges TEXT NOT NULL,
    context_ranges TEXT NOT NULL,
    primary_non_text_files TEXT NOT NULL,
    changed_lines INTEGER NOT NULL,
    PRIMARY KEY (round_id, unit_id),
    UNIQUE (round_id, ordinal)
);

CREATE TABLE schema_metadata (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    version INTEGER NOT NULL
);

CREATE TABLE supervisor_heartbeat (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    coordinator TEXT NOT NULL,
    pid INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    renewed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX evaluation_dispositions_by_run ON evaluation_dispositions (run_id, decided_at);

CREATE INDEX evaluation_usage_by_run ON evaluation_usage (run_id, recorded_at);

CREATE INDEX gate_results_by_run ON gate_results (run_id, phase, checkpoint_sha, ordinal);

CREATE INDEX ideation_commands_by_session ON ideation_commands (session_id, sequence);

CREATE INDEX ideation_commands_by_state ON ideation_commands (state, sequence);

CREATE INDEX ideation_invocations_by_session ON ideation_invocations (session_id, role, created_at);

CREATE INDEX ideation_sessions_by_repository ON ideation_sessions (repository_path, updated_at);

CREATE INDEX invocations_by_run ON invocations (run_id, updated_at);

CREATE INDEX local_execution_authorizations_by_repository ON local_execution_authorizations(repository_path,created_at,request_id);

CREATE INDEX local_execution_previews_by_repository ON local_execution_previews(repository_path,revision);

CREATE INDEX local_issue_comments_by_issue ON local_issue_comments(repository_path,issue_id,sequence);

CREATE UNIQUE INDEX one_accepted_result_per_ideation_trigger ON ideation_invocations (session_id, trigger_id) WHERE status = 'accepted';

CREATE UNIQUE INDEX one_active_invocation_per_ideation_session ON ideation_invocations (session_id) WHERE status = 'active';

CREATE UNIQUE INDEX one_active_invocation_per_review_unit ON invocations (run_id, role, review_unit_id) WHERE status = 'active';

CREATE UNIQUE INDEX one_active_run_per_repository ON operational_runs (repository_path) WHERE status NOT IN ('complete', 'cancelled', 'failed');

CREATE UNIQUE INDEX one_reserved_local_claim ON local_claims(repository_path) WHERE state='reserved';

CREATE UNIQUE INDEX review_rounds_by_run_checkpoint ON review_rounds (run_id, checkpoint_sha);

CREATE INDEX review_unit_results_by_round_role ON review_unit_results (round_id, role, unit_id);

CREATE INDEX review_units_by_round ON review_units (round_id, ordinal);

CREATE TRIGGER operational_runs_tracker_repository_frozen BEFORE UPDATE OF tracker_repository ON operational_runs WHEN OLD.tracker_repository <> '' AND NEW.tracker_repository <> OLD.tracker_repository BEGIN SELECT RAISE(ABORT, 'the frozen tracker repository of a run cannot change'); END;
