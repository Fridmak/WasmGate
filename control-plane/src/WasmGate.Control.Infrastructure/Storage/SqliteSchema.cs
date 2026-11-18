using System.Data;
using System.Globalization;
using Microsoft.Data.Sqlite;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Embedded SQLite schema for the control-plane database.
/// </summary>
/// <remarks>
/// <para>
/// The schema is the final logical shape of the PostgreSQL migrations, not a
/// replay of their individual ALTER statements. It is created once per file and
/// versioned with <c>PRAGMA user_version</c>.
/// </para>
/// <para>
/// Timestamps are Unix milliseconds in UTC. UUIDs are TEXT and JSON documents
/// are TEXT; SQLite has no native UUID or JSONB storage class.
/// </para>
/// </remarks>
internal static class SqliteSchema
{
    /// <summary>Schema version written to <c>PRAGMA user_version</c>.</summary>
    internal const int CurrentVersion = 1;

    /// <summary>Creates the schema when the database is new or incomplete.</summary>
    /// <param name="connection">Open connection to the SQLite database.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <exception cref="InvalidOperationException">
    /// The file was created by a newer control-plane schema version.
    /// </exception>
    internal static async Task EnsureAsync(
        SqliteConnection connection,
        CancellationToken cancellationToken)
    {
        await ExecuteScalarAsync(connection, "PRAGMA journal_mode = WAL;", cancellationToken);
        await ExecuteNonQueryAsync(connection, "PRAGMA foreign_keys = ON;", cancellationToken);

        var version = Convert.ToInt32(
            await ExecuteScalarAsync(connection, "PRAGMA user_version;", cancellationToken),
            CultureInfo.InvariantCulture);
        if (version > CurrentVersion)
        {
            throw new InvalidOperationException(
                $"SQLite database schema version {version} is newer than this control plane supports " +
                $"({CurrentVersion}). Upgrade the control-plane binary first.");
        }

        if (version == CurrentVersion)
        {
            return;
        }

        await using (var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false))
        {
            await ExecuteNonQueryAsync(connection, Schema, transaction, cancellationToken);
            await ExecuteNonQueryAsync(
                connection,
                """
                INSERT INTO environments (name, created_by, created_at)
                VALUES ('prod', 'system', @now), ('staging', 'system', @now)
                ON CONFLICT (name) DO NOTHING;
                """,
                transaction,
                cancellationToken,
                ("@now", DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()));
            await transaction.CommitAsync(cancellationToken);
        }

        // user_version is a header field; setting it after the DDL transaction
        // keeps the transaction itself free of PRAGMA semantics.
        await ExecuteNonQueryAsync(
            connection,
            $"PRAGMA user_version = {CurrentVersion};",
            cancellationToken);
    }

    private static async Task ExecuteNonQueryAsync(
        SqliteConnection connection,
        string sql,
        CancellationToken cancellationToken,
        params (string Name, object Value)[] parameters)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        foreach (var (name, value) in parameters)
        {
            command.Parameters.AddWithValue(name, value);
        }

        await command.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task ExecuteNonQueryAsync(
        SqliteConnection connection,
        string sql,
        SqliteTransaction transaction,
        CancellationToken cancellationToken,
        params (string Name, object Value)[] parameters)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        command.Transaction = transaction;
        foreach (var (name, value) in parameters)
        {
            command.Parameters.AddWithValue(name, value);
        }

        await command.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task<object?> ExecuteScalarAsync(
        SqliteConnection connection,
        string sql,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        return await command.ExecuteScalarAsync(cancellationToken);
    }

    private const string Schema = """
        CREATE TABLE IF NOT EXISTS environments (
            name        TEXT PRIMARY KEY,
            created_by  TEXT NOT NULL,
            created_at  INTEGER NOT NULL
        );

        CREATE TABLE IF NOT EXISTS config_versions (
            id          TEXT PRIMARY KEY,
            environment TEXT NOT NULL REFERENCES environments (name),
            sha256      TEXT NOT NULL,
            yaml        TEXT NOT NULL,
            message     TEXT,
            created_by  TEXT NOT NULL,
            created_at  INTEGER NOT NULL,
            CONSTRAINT config_versions_environment_sha256_unique UNIQUE (environment, sha256)
        );

        CREATE INDEX IF NOT EXISTS config_versions_env_created_idx
            ON config_versions (environment, created_at DESC);

        CREATE TABLE IF NOT EXISTS artifacts (
            id          TEXT PRIMARY KEY,
            kind        TEXT NOT NULL,
            size        INTEGER NOT NULL CHECK (size >= 0),
            media_type  TEXT,
            signature   TEXT,
            created_by TEXT,
            created_at  INTEGER NOT NULL
        );

        CREATE TABLE IF NOT EXISTS releases (
            id                  TEXT PRIMARY KEY,
            environment         TEXT NOT NULL REFERENCES environments (name),
            config_version_id   TEXT NOT NULL REFERENCES config_versions (id),
            config_sha256       TEXT NOT NULL,
            signature           TEXT,
            status              TEXT NOT NULL,
            created_by          TEXT NOT NULL,
            created_at          INTEGER NOT NULL
        );

        CREATE INDEX IF NOT EXISTS releases_env_created_idx
            ON releases (environment, created_at DESC);

        CREATE TABLE IF NOT EXISTS release_artifacts (
            release_id  TEXT NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
            artifact_id TEXT NOT NULL REFERENCES artifacts (id),
            name        TEXT NOT NULL,
            PRIMARY KEY (release_id, name)
        );

        CREATE TABLE IF NOT EXISTS agents (
            id                  TEXT PRIMARY KEY,
            name                TEXT NOT NULL UNIQUE,
            environment         TEXT NOT NULL DEFAULT 'prod' REFERENCES environments (name),
            version             TEXT NOT NULL,
            abi_versions        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(abi_versions)),
            labels              TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(labels)),
            status              TEXT NOT NULL,
            registered_at       INTEGER NOT NULL,
            last_seen_at        INTEGER NOT NULL,
            active_release_id   TEXT,
            active_config_sha256 TEXT
        );

        CREATE INDEX IF NOT EXISTS agents_status_last_seen_idx
            ON agents (status, last_seen_at DESC);

        CREATE INDEX IF NOT EXISTS agents_environment_idx
            ON agents (environment);

        CREATE TABLE IF NOT EXISTS deployments (
            id              TEXT PRIMARY KEY,
            environment     TEXT NOT NULL REFERENCES environments (name),
            release_id      TEXT NOT NULL REFERENCES releases (id),
            status          TEXT NOT NULL,
            created_by      TEXT NOT NULL,
            created_at      INTEGER NOT NULL,
            completed_at    INTEGER,
            message         TEXT
        );

        CREATE INDEX IF NOT EXISTS deployments_env_status_idx
            ON deployments (environment, status, created_at DESC);

        CREATE TABLE IF NOT EXISTS deployment_waves (
            deployment_id TEXT NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
            "index"       INTEGER NOT NULL CHECK ("index" >= 0),
            selector      TEXT NOT NULL DEFAULT '*',
            status        TEXT NOT NULL,
            started_at    INTEGER,
            completed_at  INTEGER,
            PRIMARY KEY (deployment_id, "index")
        );

        CREATE TABLE IF NOT EXISTS audit_events (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            at          INTEGER NOT NULL,
            actor       TEXT NOT NULL,
            action      TEXT NOT NULL,
            target      TEXT NOT NULL,
            details     TEXT,
            request_id  TEXT
        );

        CREATE INDEX IF NOT EXISTS audit_events_at_idx
            ON audit_events (at DESC);

        CREATE TABLE IF NOT EXISTS enrollment_tokens (
            id          TEXT PRIMARY KEY,
            token_hash  TEXT NOT NULL,
            agent_id    TEXT REFERENCES agents (id) ON DELETE CASCADE,
            environment TEXT REFERENCES environments (name),
            name_prefix TEXT,
            max_uses    INTEGER NOT NULL CHECK (max_uses > 0),
            used_count  INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0),
            expires_at  INTEGER NOT NULL,
            created_at  INTEGER NOT NULL,
            CONSTRAINT enrollment_tokens_hash_unique UNIQUE (token_hash)
        );

        CREATE INDEX IF NOT EXISTS enrollment_tokens_expires_idx
            ON enrollment_tokens (expires_at);

        CREATE TABLE IF NOT EXISTS agent_certificates (
            agent_id      TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
            serial_number TEXT NOT NULL,
            thumbprint    TEXT NOT NULL,
            pem           TEXT NOT NULL,
            not_before    INTEGER NOT NULL,
            not_after     INTEGER NOT NULL,
            issued_at     INTEGER NOT NULL,
            revoked_at    INTEGER,
            PRIMARY KEY (agent_id, serial_number)
        );

        CREATE INDEX IF NOT EXISTS agent_certificates_thumbprint_idx
            ON agent_certificates (thumbprint);

        CREATE TABLE IF NOT EXISTS deleted_agents (
            id         TEXT PRIMARY KEY,
            deleted_at INTEGER NOT NULL
        );
        """;
}
