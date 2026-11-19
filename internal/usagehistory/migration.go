package usagehistory

import (
	"database/sql"
	"fmt"
)

func prepareSchema(db *sql.DB) error {
	tx, errBegin := db.Begin()
	if errBegin != nil {
		return errBegin
	}
	defer func() { _ = tx.Rollback() }()
	if _, errSchema := tx.Exec(schema); errSchema != nil {
		return errSchema
	}
	for _, table := range []string{"usage_minute", "usage_hour"} {
		if errMigrate := migrateProjectColumn(tx, table); errMigrate != nil {
			return fmt.Errorf("migrate %s: %w", table, errMigrate)
		}
	}
	return tx.Commit()
}

// SQLite cannot add a column to a primary key in place. Rebuild both rollups
// in one transaction, leaving legacy rows in the unassigned project group.
func migrateProjectColumn(tx *sql.Tx, table string) error {
	var hasProject bool
	if errCheck := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = 'project')`, table).Scan(&hasProject); errCheck != nil || hasProject {
		return errCheck
	}
	columns := "bucket, provider, model, auth_index, tier, " + counterColumns
	for _, statement := range []string{
		`CREATE TABLE ` + table + `_with_project` + rollupSchema,
		`INSERT INTO ` + table + `_with_project (` + columns + `) SELECT ` + columns + ` FROM ` + table,
		`DROP TABLE ` + table,
		`ALTER TABLE ` + table + `_with_project RENAME TO ` + table,
	} {
		if _, errExec := tx.Exec(statement); errExec != nil {
			return errExec
		}
	}
	return nil
}
