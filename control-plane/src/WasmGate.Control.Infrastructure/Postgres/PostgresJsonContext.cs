using System.Text.Json.Serialization;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>
/// Source-generated JSON contracts used by the PostgreSQL adapters.
/// </summary>
/// <remarks>
/// Values written to <c>jsonb</c> columns are serialized through this context so
/// that the adapters stay reflection-free and Native AOT compatible.
/// </remarks>
[JsonSerializable(typeof(Dictionary<string, string>))]
internal sealed partial class PostgresJsonContext : JsonSerializerContext;
