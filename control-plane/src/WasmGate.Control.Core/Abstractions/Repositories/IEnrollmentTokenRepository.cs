using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Stores enrollment tokens.</summary>
/// <remarks>
/// Only the SHA-256 of a token is stored, so a leaked database does not expose
/// usable tokens. A token may be bound to one agent (per-agent enrollment) or
/// scoped by a name prefix and reuse count (registration token).
/// </remarks>
public interface IEnrollmentTokenRepository
{
    /// <summary>Stores a new enrollment token.</summary>
    /// <param name="token">Token record to persist; contains the hash, not the raw value.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken);

    /// <summary>Finds a token by its SHA-256 hash.</summary>
    /// <param name="tokenHash">Lower-case hex SHA-256 of the raw token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The token, or null when it does not exist.</returns>
    Task<EnrollmentToken?> FindByHashAsync(string tokenHash, CancellationToken cancellationToken);

    /// <summary>Reserves one use of a token if a use is still available.</summary>
    /// <param name="id">Token identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Must be a single conditional update
    /// (<c>used_count = used_count + 1 WHERE used_count &lt; max_uses</c>).
    /// Two agents may enroll with the same registration token at the same time,
    /// so a read-then-write check would exceed the limit.
    /// </remarks>
    /// <returns>True when a use was reserved; false when the token is missing or exhausted.</returns>
    Task<bool> ConsumeAsync(Guid id, CancellationToken cancellationToken);
}
