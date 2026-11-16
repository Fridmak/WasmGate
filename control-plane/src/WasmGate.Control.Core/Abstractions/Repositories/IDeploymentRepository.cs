using WasmGate.Control.Core.Entities.Delivery;

namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>Stores deployment requests and their wave progress.</summary>
public interface IDeploymentRepository
{
    /// <summary>Stores a new deployment together with its waves.</summary>
    /// <param name="deployment">Deployment to persist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>The header and all waves are written in one transaction.</remarks>
    Task AddAsync(Deployment deployment, CancellationToken cancellationToken);

    /// <summary>Finds a deployment by its identifier.</summary>
    /// <param name="id">Deployment identifier.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The deployment with its waves, or null when it does not exist.</returns>
    Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken);

    /// <summary>Lists deployments that have not reached a terminal state.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Pending and running deployments, newest first.</returns>
    Task<IReadOnlyList<Deployment>> ListActiveAsync(string environment, CancellationToken cancellationToken);

    /// <summary>Persists changed deployment status and timestamps.</summary>
    /// <param name="deployment">Deployment with updated state.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>The header and all waves are updated in one transaction.</remarks>
    Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken);
}
