using Microsoft.Data.Sqlite;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// Translates SQLite constraint errors into the same domain situations the
/// repositories report.
/// </summary>
/// <remarks>
/// SQLite reports a foreign-key failure as extended code 787. The primary code
/// is the generic constraint code, so the extended code is the stable check.
/// </remarks>
internal static class SqliteErrors
{
    private const int ForeignKeyConstraint = 787;
    private const int UniqueConstraint = 2067;
    private const int PrimaryKeyConstraint = 1555;

    /// <summary>Returns true for a foreign-key violation.</summary>
    /// <param name="exception">Exception thrown by Microsoft.Data.Sqlite.</param>
    /// <returns>True when the constraint is <c>FOREIGN KEY</c>.</returns>
    public static bool IsForeignKeyViolation(SqliteException exception) =>
        exception.SqliteExtendedErrorCode == ForeignKeyConstraint;

    /// <summary>Returns true for a unique-index or primary-key violation.</summary>
    /// <param name="exception">Exception thrown by Microsoft.Data.Sqlite.</param>
    /// <returns>True when the constraint is unique.</returns>
    public static bool IsUniqueViolation(SqliteException exception) =>
        exception.SqliteExtendedErrorCode is UniqueConstraint or PrimaryKeyConstraint;
}
