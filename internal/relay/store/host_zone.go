package store

// Only new side tables. The frozen v1 schema and its rows retain their meaning.
// Pre-host binaries can open the database and ignore these tables; they must not
// be used as controllers of an enabled host pool. The runtime swap gate refuses
// dropping these objects on downgrade.
var hostZone = []string{
	`CREATE TABLE IF NOT EXISTS executor_hosts (
 host_id TEXT PRIMARY KEY,
 config_json TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
 session_limit INTEGER NOT NULL DEFAULT 1 CHECK(session_limit > 0),
 test_slots INTEGER NOT NULL DEFAULT 1 CHECK(test_slots > 0),
 test_cpu_budget INTEGER NOT NULL DEFAULT 0 CHECK(test_cpu_budget >= 0),
 observation_json TEXT NOT NULL DEFAULT '{}',
 received_at TEXT NOT NULL DEFAULT '',
 checked INTEGER NOT NULL DEFAULT 0 CHECK(checked IN (0,1))
 )`,
	`CREATE TABLE IF NOT EXISTS executor_host_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 host_id TEXT NOT NULL REFERENCES executor_hosts(host_id),
 kind TEXT NOT NULL,
 detail_json TEXT NOT NULL,
 recorded_at TEXT NOT NULL
 )`,
	`CREATE TABLE IF NOT EXISTS executor_host_incarnations (
 host_id TEXT NOT NULL REFERENCES executor_hosts(host_id),
 incarnation TEXT NOT NULL,
 boot_id TEXT NOT NULL,
 PRIMARY KEY(host_id,incarnation)
 )`,
	`CREATE TABLE IF NOT EXISTS executor_assignments (
 execution_id TEXT PRIMARY KEY,
 plan_id TEXT NOT NULL,
 node_id TEXT NOT NULL,
 host_id TEXT NOT NULL REFERENCES executor_hosts(host_id),
 incarnation TEXT NOT NULL,
 task_digest TEXT NOT NULL,
 admission_json TEXT NOT NULL,
 grant_json TEXT NOT NULL,
 slot_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('assigned','start_unknown','running','cancel_unknown','terminated','not_started')),
 proof TEXT NOT NULL DEFAULT '',
 placement_json TEXT NOT NULL,
 recorded_at TEXT NOT NULL
 )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS executor_assignment_one_live_node ON executor_assignments(plan_id,node_id)
 WHERE state NOT IN ('terminated','not_started')`,
	`CREATE TABLE IF NOT EXISTS executor_heavy_permits (
 permit_id TEXT PRIMARY KEY,
 execution_id TEXT NOT NULL REFERENCES executor_assignments(execution_id),
 command_id TEXT NOT NULL,
 cpu INTEGER NOT NULL CHECK(cpu > 0),
 state TEXT NOT NULL CHECK(state IN ('held','terminated')),
 proof TEXT NOT NULL DEFAULT '',
 UNIQUE(execution_id,command_id)
 )`,
	`CREATE TABLE IF NOT EXISTS executor_toolchain_plans (
 plan_id TEXT PRIMARY KEY,
 host_id TEXT NOT NULL REFERENCES executor_hosts(host_id),
 manifest_json TEXT NOT NULL,
 profile_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('planned','applying','applied')),
 readback_json TEXT NOT NULL DEFAULT '{}',
 recorded_at TEXT NOT NULL
 )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS executor_one_applying_toolchain ON executor_toolchain_plans(host_id) WHERE state='applying'`,
}
