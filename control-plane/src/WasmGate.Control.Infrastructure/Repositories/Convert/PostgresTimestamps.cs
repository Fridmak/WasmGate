namespace WasmGate.Control.Infrastructure.Repositories.Convert;

/// <summary>
/// Converts between domain <see cref="DateTimeOffset"/> values and the
/// <see cref="DateTime"/> values Dapper.AOT moves in and out of PostgreSQL
/// <c>timestamptz</c> columns.
/// </summary>
/// <remarks>
/// Npgsql returns <c>timestamptz</c> as a UTC <see cref="DateTime"/>, and
/// Dapper.AOT cannot convert it to <see cref="DateTimeOffset"/>: reading such a
/// column into a <see cref="DateTimeOffset"/> property throws
/// <see cref="InvalidCastException"/>. Every adapter therefore reads
/// <see cref="DateTime"/> and converts explicitly, which also keeps the mapping
/// independent of Npgsql timestamp settings.
/// </remarks>
internal static class PostgresTimestamps
{
    /// <summary>Converts a timestamp to the UTC value sent to PostgreSQL.</summary>
    /// <param name="value">Timestamp from a domain entity.</param>
    /// <returns>The same instant as a UTC <see cref="DateTime"/>.</returns>
    public static DateTime ToParameter(DateTimeOffset value) => value.UtcDateTime;

    /// <summary>Converts a nullable timestamp to the UTC value sent to PostgreSQL.</summary>
    /// <param name="value">Timestamp from a domain entity, or null.</param>
    /// <returns>The same instant as a UTC <see cref="DateTime"/>, or null.</returns>
    public static DateTime? ToParameter(DateTimeOffset? value) => value?.UtcDateTime;

    /// <summary>Converts a <c>timestamptz</c> column to a domain timestamp.</summary>
    /// <param name="value">Value read from the database.</param>
    /// <returns>The instant as <see cref="DateTimeOffset"/> with a zero offset.</returns>
    public static DateTimeOffset FromColumn(DateTime value) =>
        new(DateTime.SpecifyKind(value, DateTimeKind.Utc));

    /// <summary>Converts a nullable <c>timestamptz</c> column to a domain timestamp.</summary>
    /// <param name="value">Value read from the database, or null.</param>
    /// <returns>The instant as <see cref="DateTimeOffset"/>, or null.</returns>
    public static DateTimeOffset? FromColumn(DateTime? value) =>
        value is { } timestamp ? FromColumn(timestamp) : null;
}
