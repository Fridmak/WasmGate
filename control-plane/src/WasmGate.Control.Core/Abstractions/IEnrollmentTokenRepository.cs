using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Stores enrollment tokens. Tokens are one-time and short-lived.</summary>
public interface IEnrollmentTokenRepository
{
    /// <summary>Stores a new enrollment token.</summary>
    /// <param name="token">Token record to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken);

    /// <summary>Finds a token by its SHA-256 hash.</summary>
    /// <param name="tokenHash">SHA-256 hex of the raw token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The token, or null when it does not exist.</returns>
    Task<EnrollmentToken?> FindByHashAsync(string tokenHash, CancellationToken cancellationToken);

    /// <summary>
    /// Atomically increments the token usage counter if the token still has a
    /// free use.
    /// </summary>
    /// <param name="id">Token identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// A PostgreSQL implementation must use a single conditional update, for
    /// example:
    /// <c>UPDATE enrollment_tokens SET used_count = used_count + 1
    /// WHERE id = @id AND used_count &lt; max_uses RETURNING true</c>.
    /// Read-then-write is not sufficient because two agents may enroll at the
    /// same time with the same registration token.
    /// </remarks>
    /// <returns>True when a use was reserved; false when the token is missing or exhausted.</returns>
    Task<bool> ConsumeAsync(Guid id, CancellationToken cancellationToken);
}
