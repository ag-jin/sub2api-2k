package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"os"
	"os/exec"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestRunMigrationsWithDSN_RequiresDedicatedDSN(t *testing.T) {
	called := false
	err := runMigrationsWithDSN(context.Background(), "", func(_, _ string) (*sql.DB, error) {
		called = true
		return nil, nil
	}, func(context.Context, *sql.DB) error {
		return nil
	})

	require.EqualError(t, err, "MIGRATION_DATABASE_DSN is required for --migrate-only")
	require.False(t, called)
}

func TestRunMigrationsOnlyDoesNotFallBackToRuntimeDatabaseConfiguration(t *testing.T) {
	t.Setenv(migrationDatabaseDSNEnv, "")
	t.Setenv("DATABASE_HOST", "runtime-database")

	err := runMigrationsOnly()

	require.EqualError(t, err, "MIGRATION_DATABASE_DSN is required for --migrate-only")
}

func TestRunMigrationsWithDSN_AppliesAndCloses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	mock.ExpectClose()

	called := false
	err = runMigrationsWithDSN(context.Background(), "host=isolated dbname=clone", func(driver, dsn string) (*sql.DB, error) {
		require.Equal(t, "postgres", driver)
		require.Equal(t, "host=isolated dbname=clone", dsn)
		return db, nil
	}, func(ctx context.Context, got *sql.DB) error {
		called = true
		require.NotNil(t, ctx)
		require.Same(t, db, got)
		return nil
	})

	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMigrateOnlyCommandUsesEnvironmentAndExitsBeforeServerStartup(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMigrateOnlyCommandProcess")
	cmd.Env = append(os.Environ(),
		"SUB2API_TEST_MIGRATE_ONLY_PROCESS=1",
		"MIGRATION_DATABASE_DSN=host=127.0.0.1 port=1 dbname=clone connect_timeout=1 sslmode=disable",
	)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()

	require.Error(t, err)
	body := output.String()
	require.Contains(t, body, "Migration-only failed: apply database migrations")
	for _, unexpected := range []string{"Server started", "Setup wizard", "Auto setup"} {
		require.NotContains(t, body, unexpected)
	}
}

func TestMigrateOnlyCommandProcess(t *testing.T) {
	if os.Getenv("SUB2API_TEST_MIGRATE_ONLY_PROCESS") != "1" {
		return
	}

	os.Args = []string{"sub2api", "--migrate-only"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
}

func TestRunMigrationsWithDSN_DoesNotExposeDSNInErrors(t *testing.T) {
	const dsn = "host=isolated dbname=clone"

	t.Run("open", func(t *testing.T) {
		err := runMigrationsWithDSN(context.Background(), dsn, func(_, _ string) (*sql.DB, error) {
			return nil, errors.New(dsn)
		}, func(context.Context, *sql.DB) error {
			return nil
		})

		require.EqualError(t, err, "open migration database connection")
		require.NotContains(t, err.Error(), dsn)
	})

	t.Run("runner", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		mock.ExpectClose()

		err = runMigrationsWithDSN(context.Background(), dsn, func(_, _ string) (*sql.DB, error) {
			return db, nil
		}, func(context.Context, *sql.DB) error {
			return errors.New(dsn)
		})

		require.EqualError(t, err, "apply database migrations")
		require.NotContains(t, err.Error(), dsn)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
