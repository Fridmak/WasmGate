namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Converts between domain timestamps and the Unix-millisecond values stored in
/// SQLite.
/// </summary>
/// <remarks>
/// SQLite has no timestamp type. Mapping <c>TEXT</c> directly to
/// <see cref="DateTime"/> is timezone-sensitive and mapping it to
/// <see cref="DateTimeOffset"/> throws, so the store uses integers and converts
/// explicitly. Millisecond precision is enough for control-plane ordering;
/// queries that can tie add a stable identifier as a secondary key.
/// </remarks>
internal static class SqliteTimestamps
{
    /// <summary>Converts a domain timestamp to a database parameter.</summary>
    /// <param name="value">Timestamp to store.</param>
    /// <returns>Unix milliseconds in UTC.</returns>
    public static long ToParameter(DateTimeOffset value) => value.ToUnixTimeMilliseconds();

    /// <summary>Converts a nullable domain timestamp to a database parameter.</summary>
    /// <param name="value">Timestamp to store, or null.</param>
    /// <returns>Unix milliseconds, or null.</returns>
    public static long? ToParameter(DateTimeOffset? value) => value?.ToUnixTimeMilliseconds();

    /// <summary>Converts a database value to a domain timestamp.</summary>
    /// <param name="value">Unix milliseconds stored in the database.</param>
    /// <returns>Timestamp with a zero offset.</returns>
    public static DateTimeOffset FromColumn(long value) =>
        DateTimeOffset.FromUnixTimeMilliseconds(value);

    /// <summary>Converts a nullable database value to a domain timestamp.</summary>
    /// <param name="value">Unix milliseconds, or null.</param>
    /// <returns>Timestamp with a zero offset, or null.</returns>
    public static DateTimeOffset? FromColumn(long? value) =>
        value is { } timestamp ? FromColumn(timestamp) : null;
}
