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

    /// <summary>Increments the token usage counter.</summary>
    /// <param name="id">Token identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task ConsumeAsync(Guid id, CancellationToken cancellationToken);
}
