using System.Text.Json;
using WasmGate.Control.Infrastructure.Repositories.Serialization;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// JSON encoding helpers shared by the SQLite repositories.
/// </summary>
/// <remarks>
/// The PostgreSQL adapters use JSONB/array columns and Npgsql conversion. SQLite
/// stores both as TEXT, so the SQLite adapters serialize and parse explicitly
/// through the source-generated <see cref="StorageJsonContext"/>.
/// </remarks>
internal static class SqliteJson
{
    /// <summary>Serializes agent labels as a JSON object.</summary>
    /// <param name="labels">Labels reported by the agent.</param>
    /// <returns>A JSON object with the same entries.</returns>
    public static string SerializeLabels(IReadOnlyDictionary<string, string> labels) =>
        JsonSerializer.Serialize(
            new Dictionary<string, string>(labels, StringComparer.Ordinal),
            StorageJsonContext.Default.DictionaryStringString);

    /// <summary>Parses agent labels from a JSON object.</summary>
    /// <param name="json">JSON text stored in the database.</param>
    /// <returns>A non-null, case-sensitive dictionary.</returns>
    public static IReadOnlyDictionary<string, string> ParseLabels(string json) =>
        JsonSerializer.Deserialize(json, StorageJsonContext.Default.DictionaryStringString)
        ?? new Dictionary<string, string>(StringComparer.Ordinal);

    /// <summary>Serializes ABI versions as a JSON array.</summary>
    /// <param name="versions">ABI versions reported by the agent.</param>
    /// <returns>A JSON array of strings.</returns>
    public static string SerializeAbiVersions(IReadOnlyList<string> versions) =>
        JsonSerializer.Serialize(versions.ToArray(), StorageJsonContext.Default.StringArray);

    /// <summary>Parses ABI versions from a JSON array.</summary>
    /// <param name="json">JSON text stored in the database.</param>
    /// <returns>A non-null list of versions.</returns>
    public static IReadOnlyList<string> ParseAbiVersions(string json) =>
        JsonSerializer.Deserialize(json, StorageJsonContext.Default.StringArray) ?? [];
}
