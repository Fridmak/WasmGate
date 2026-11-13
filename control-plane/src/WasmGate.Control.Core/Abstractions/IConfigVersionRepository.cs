using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Stores immutable configuration versions.</summary>
public interface IConfigVersionRepository
{
    /// <summary>Stores a new configuration version.</summary>
    /// <param name="version">Configuration version to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task AddAsync(ConfigVersion version, CancellationToken cancellationToken);

    /// <summary>Finds a configuration version by its identifier.</summary>
    /// <param name="id">Configuration version identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The version, or null when it does not exist.</returns>
    Task<ConfigVersion?> FindAsync(Guid id, CancellationToken cancellationToken);

    /// <summary>Finds a configuration version by its SHA-256.</summary>
    /// <param name="sha256">Lower-case hex SHA-256 of the YAML document.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The version, or null when it does not exist.</returns>
    Task<ConfigVersion?> FindByHashAsync(string sha256, CancellationToken cancellationToken);

    /// <summary>Lists configuration versions for an environment, newest first.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Configuration versions belonging to the environment.</returns>
    Task<IReadOnlyList<ConfigVersion>> ListAsync(string environment, CancellationToken cancellationToken);
}
