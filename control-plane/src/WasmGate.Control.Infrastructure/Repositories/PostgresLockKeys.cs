namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// Stable PostgreSQL advisory-lock keys shared by repositories and services.
/// </summary>
/// <remarks>
/// The keys are namespaced strings hashed by PostgreSQL with
/// <c>hashtextextended</c>. Every writer that must serialize with a deletion
/// uses the same key.
/// </remarks>
internal static class PostgresLockKeys
{
    /// <summary>Lock key for one agent.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <returns>A namespaced key.</returns>
    public static string Agent(Guid id) => $"wasmgate:agent:{id:D}";

    /// <summary>Lock key for one environment.</summary>
    /// <param name="name">Normalised environment name.</param>
    /// <returns>A namespaced key.</returns>
    public static string Environment(string name) => $"wasmgate:env:{name}";
}
