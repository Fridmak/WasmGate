using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory enrollment token store. Replace with PostgreSQL for production.</summary>
public sealed class InMemoryEnrollmentTokenRepository : IEnrollmentTokenRepository
{
    private readonly ConcurrentDictionary<Guid, EnrollmentToken> _tokens = new();

    /// <inheritdoc />
    public Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _tokens[token.Id] = token;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<EnrollmentToken?> FindByHashAsync(string tokenHash, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        var match = _tokens.Values.FirstOrDefault(t =>
            string.Equals(t.TokenHash, tokenHash, StringComparison.OrdinalIgnoreCase));
        return Task.FromResult(match);
    }

    /// <inheritdoc />
    public Task ConsumeAsync(Guid id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        if (_tokens.TryGetValue(id, out var token))
        {
            token.UsedCount += 1;
        }

        return Task.CompletedTask;
    }
}
