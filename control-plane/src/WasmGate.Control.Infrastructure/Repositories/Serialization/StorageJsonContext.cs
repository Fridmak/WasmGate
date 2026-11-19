using System.Text.Json.Serialization;

namespace WasmGate.Control.Infrastructure.Repositories.Serialization;

/// <summary>
/// Source-generated JSON contracts used by the storage adapters.
/// </summary>
/// <remarks>
/// JSON columns are stored as text in SQLite. Serializing through a source
/// generator keeps the adapters reflection-free and Native AOT compatible.
/// </remarks>
[JsonSerializable(typeof(Dictionary<string, string>))]
[JsonSerializable(typeof(string[]))]
[JsonSerializable(typeof(CertificateRotationAudit))]
internal sealed partial class StorageJsonContext : JsonSerializerContext;

/// <summary>Audit details written when a certificate is replaced.</summary>
internal sealed class CertificateRotationAudit
{
    /// <summary>Reason for retiring the old certificate.</summary>
    [JsonPropertyName("reason")]
    public string Reason { get; init; } = string.Empty;

    /// <summary>Serial number of the replacement certificate.</summary>
    [JsonPropertyName("replacedBy")]
    public string ReplacedBy { get; init; } = string.Empty;
}
