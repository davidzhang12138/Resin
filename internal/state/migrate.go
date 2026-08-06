package state

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratedb "github.com/golang-migrate/migrate/v4/database"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const (
	stateMigrationsPath = "migrations/state"
	cacheMigrationsPath = "migrations/cache"

	// Keep these version markers in sync with SQL files under migrations/state/.
	// stateLegacyBaselineVersion must remain fixed to the highest migration
	// version covered by compatibility detection for pre-migrate databases.
	//
	// NOTE: 7 is our local migration (max_node_reference_latency). The upstream
	// migration series (endpoints=7, enabled=8, regex rules=9) collides with it,
	// so upstream migrations are renumbered to 10/11/12 on this fork. See the
	// 000010_/000011_/000012_ SQL files under migrations/state/.
	stateVersionBaseSchema                         = 1
	stateVersionAddEmptyAccountBehavior            = 2
	stateVersionAddFixedAccountHeader              = 3
	stateVersionNormalizeMissAction                = 4
	stateVersionAddIncrementalAliveNodes           = 5
	stateVersionAddPassiveCircuitBreakerDisabled   = 6
	stateVersionAddPlatformMaxNodeReferenceLatency = 7
	stateUpstreamVersionAddEndpointEnabled         = 8
	stateUpstreamVersionPlatformRegexFilterRules   = 9
	stateVersionAddEndpoints                       = 10
	stateVersionAddEndpointEnabled                 = 11
	stateVersionPlatformRegexFilterRules           = 12
	stateLatestVersion                             = stateVersionPlatformRegexFilterRules
	stateLegacyBaselineVersion                     = stateVersionAddFixedAccountHeader

	stateBaseSchemaMigration = stateMigrationsPath + "/000001_state_base.up.sql"
)

//go:embed migrations/state/*.sql migrations/cache/*.sql
var migrationsFS embed.FS

type preMigrateHook func(db *sql.DB, driver migratedb.Driver) error

// MigrateStateDB applies state.db migrations.
func MigrateStateDB(db *sql.DB) error {
	return migrateSQLiteDB(db, stateMigrationsPath, migrateDefaultTable, prepareStateMigrationCompatibility)
}

// MigrateCacheDB applies cache.db migrations.
func MigrateCacheDB(db *sql.DB) error {
	return migrateSQLiteDB(db, cacheMigrationsPath, migrateDefaultTable, nil)
}

const migrateDefaultTable = "schema_migrations"

func migrateSQLiteDB(db *sql.DB, fsPath, migrationsTable string, preHook preMigrateHook) error {
	if db == nil {
		return fmt.Errorf("migrate %s: nil db", fsPath)
	}

	sourceDriver, err := iofs.New(migrationsFS, fsPath)
	if err != nil {
		return fmt.Errorf("migrate %s: init source: %w", fsPath, err)
	}

	dbDriver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{
		MigrationsTable: migrationsTable,
	})
	if err != nil {
		return fmt.Errorf("migrate %s: init db driver: %w", fsPath, err)
	}

	if preHook != nil {
		if err := preHook(db, dbDriver); err != nil {
			return fmt.Errorf("migrate %s: prehook: %w", fsPath, err)
		}
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "sqlite", dbDriver)
	if err != nil {
		return fmt.Errorf("migrate %s: init migrator: %w", fsPath, err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate %s: up: %w", fsPath, err)
	}
	return nil
}

// prepareStateMigrationCompatibility handles databases produced by the
// upstream migration series before it was renumbered on this fork. Version 8
// already contains endpoints.enabled, while version 9 also already contains
// the regex-filter data conversion. Letting the fork's 000011/000012
// migrations run again would either fail on the existing column or transform
// the regex prefixes twice.
func prepareStateMigrationCompatibility(db *sql.DB, driver migratedb.Driver) error {
	if err := prepareLegacyStateBaseline(db, driver); err != nil {
		return err
	}

	version, dirty, hasVersion, err := readMigrationState(db, migrateDefaultTable)
	if err != nil {
		return err
	}
	if !hasVersion || dirty {
		return nil
	}

	hasEndpoints, err := hasTable(db, "endpoints")
	if err != nil {
		return err
	}
	if !hasEndpoints {
		return nil
	}
	hasEnabled, err := hasTableColumn(db, "endpoints", "enabled")
	if err != nil {
		return err
	}
	if !hasEnabled {
		return nil
	}

	switch version {
	case stateUpstreamVersionAddEndpointEnabled:
		// Upstream 000008 added endpoints.enabled. The fork's 000012 still
		// needs to convert legacy regex filters, so continue from 000011.
		return setMigrationVersion(driver, stateVersionAddEndpointEnabled)
	case stateUpstreamVersionPlatformRegexFilterRules:
		// Upstream 000009 already converted regex filters. Skip the fork's
		// equivalent migration to avoid adding '*' prefixes a second time.
		return setMigrationVersion(driver, stateLatestVersion)
	default:
		return nil
	}
}

// prepareLegacyStateBaseline aligns migration version metadata for databases
// created before golang-migrate was introduced.
func prepareLegacyStateBaseline(db *sql.DB, driver migratedb.Driver) error {
	hasVersion, err := hasMigrationVersion(db, migrateDefaultTable)
	if err != nil {
		return err
	}
	if hasVersion {
		return nil
	}

	hasPlatforms, err := hasTable(db, "platforms")
	if err != nil {
		return err
	}
	if !hasPlatforms {
		return nil
	}

	hasEmptyBehavior, err := hasTableColumn(db, "platforms", "reverse_proxy_empty_account_behavior")
	if err != nil {
		return err
	}
	hasFixedHeader, err := hasTableColumn(db, "platforms", "reverse_proxy_fixed_account_header")
	if err != nil {
		return err
	}
	hasPassiveCircuitBreakerDisabled, err := hasTableColumn(db, "platforms", "passive_circuit_breaker_disabled")
	if err != nil {
		return err
	}
	hasMaxNodeReferenceLatency, err := hasTableColumn(db, "platforms", "max_node_reference_latency_ns")
	if err != nil {
		return err
	}
	hasIncrementalAliveNodes, err := hasTableColumn(db, "subscriptions", "incremental_alive_nodes")
	if err != nil {
		return err
	}

	switch {
	case hasEmptyBehavior && hasFixedHeader && hasIncrementalAliveNodes && hasPassiveCircuitBreakerDisabled && hasMaxNodeReferenceLatency:
		return setLegacyMigrationVersion(db, driver, stateVersionAddPlatformMaxNodeReferenceLatency)
	case hasEmptyBehavior && hasFixedHeader && hasIncrementalAliveNodes && hasPassiveCircuitBreakerDisabled:
		return setLegacyMigrationVersion(db, driver, stateVersionAddPassiveCircuitBreakerDisabled)
	case hasEmptyBehavior && hasFixedHeader && hasIncrementalAliveNodes:
		return setLegacyMigrationVersion(db, driver, stateVersionAddIncrementalAliveNodes)
	case hasEmptyBehavior && hasFixedHeader:
		return setLegacyMigrationVersion(db, driver, stateLegacyBaselineVersion)
	case hasEmptyBehavior && !hasFixedHeader:
		return setLegacyMigrationVersion(db, driver, stateVersionAddEmptyAccountBehavior)
	case !hasEmptyBehavior && hasFixedHeader:
		// This mixed state should not happen in normal upgrades. Repair it once.
		if err := ensureTableColumn(
			db,
			"platforms",
			"reverse_proxy_empty_account_behavior",
			`reverse_proxy_empty_account_behavior TEXT NOT NULL DEFAULT 'RANDOM'`,
		); err != nil {
			return err
		}
		return setLegacyMigrationVersion(db, driver, stateLegacyBaselineVersion)
	default:
		// No baseline metadata: migrate from base schema.
		return nil
	}
}

func hasMigrationVersion(db *sql.DB, table string) (bool, error) {
	_, _, hasVersion, err := readMigrationState(db, table)
	return hasVersion, err
}

func readMigrationState(db *sql.DB, table string) (version int, dirty, hasVersion bool, err error) {
	var rawVersion uint64
	err = db.QueryRow(fmt.Sprintf("SELECT version, dirty FROM %s LIMIT 1", table)).Scan(&rawVersion, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("read %s: %w", table, err)
	}
	if rawVersion > uint64(^uint(0)>>1) {
		return 0, false, false, fmt.Errorf("read %s: migration version %d overflows int", table, rawVersion)
	}
	return int(rawVersion), dirty, true, nil
}

func setMigrationVersion(driver migratedb.Driver, version int) error {
	if err := driver.SetVersion(version, false); err != nil {
		return fmt.Errorf("set migration version=%d: %w", version, err)
	}
	return nil
}

func setLegacyMigrationVersion(db *sql.DB, driver migratedb.Driver, version int) error {
	if err := ensureStateBaseSchema(db); err != nil {
		return err
	}
	return setMigrationVersion(driver, version)
}

func ensureStateBaseSchema(db *sql.DB) error {
	schema, err := migrationsFS.ReadFile(stateBaseSchemaMigration)
	if err != nil {
		return fmt.Errorf("read state base schema: %w", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		return fmt.Errorf("ensure state base schema: %w", err)
	}
	return nil
}

func hasTable(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup table %s: %w", table, err)
	}
	return true, nil
}
