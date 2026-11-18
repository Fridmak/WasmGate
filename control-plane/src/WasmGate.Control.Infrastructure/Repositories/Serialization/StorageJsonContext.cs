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
internal sealed partial class StorageJsonContext : JsonSerializerContext;
