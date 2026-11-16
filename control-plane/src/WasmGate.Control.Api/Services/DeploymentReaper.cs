using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Delivery;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Closes deployments that can no longer receive a result.
/// </summary>
/// <remarks>
/// A deployment row is written before commands are dispatched. If the control
/// plane crashes in between, or an agent never answers, nothing else would move
/// the row out of <c>Running</c>. The reaper marks such deployments and their
/// unfinished waves as failed after <see cref="ControlPlaneOptions.DeploymentTimeout"/>,
/// so operators and API consumers see a terminal state.
/// </remarks>
internal sealed class DeploymentReaper(
    IDeploymentRepository deployments,
    IAuditLog audit,
    ControlPlaneOptions options,
    ILogger<DeploymentReaper> logger) : BackgroundService
{
    /// <summary>How often the reaper looks for stale deployments.</summary>
    private static readonly TimeSpan Interval = TimeSpan.FromSeconds(30);

    /// <inheritdoc />
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var timer = new PeriodicTimer(Interval);
        try
        {
            while (await timer.WaitForNextTickAsync(stoppingToken))
            {
                try
                {
                    await ReapAsync(stoppingToken);
                }
                catch (Exception ex) when (ex is not OperationCanceledException)
                {
                    logger.LogError(ex, "deployment reaper iteration failed");
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Normal shutdown.
        }
    }

    /// <summary>Marks every running deployment older than the timeout as failed.</summary>
    /// <param name="cancellationToken">Stops the iteration on shutdown.</param>
    private async Task ReapAsync(CancellationToken cancellationToken)
    {
        var cutoff = DateTimeOffset.UtcNow - options.DeploymentTimeout;
        var stale = await deployments.ListStaleRunningAsync(cutoff, cancellationToken);

        foreach (var deployment in stale)
        {
            var now = DateTimeOffset.UtcNow;
            deployment.Status = DeploymentStatus.Failed;
            deployment.CompletedAt = now;
            deployment.Message =
                $"No apply result was received within {options.DeploymentTimeout}.";

            foreach (var wave in deployment.Waves.Where(
                         wave => wave.Status is DeploymentWaveStatus.Pending or DeploymentWaveStatus.Running))
            {
                wave.Status = DeploymentWaveStatus.Failed;
                wave.CompletedAt = now;
            }

            await deployments.UpdateAsync(deployment, cancellationToken);
            await audit.AppendAsync(
                new AuditEvent
                {
                    Actor = "deployment-reaper",
                    Action = "deployment.timeout",
                    Target = deployment.Id.ToString("D"),
                },
                cancellationToken);

            logger.LogWarning(
                "deployment {DeploymentId} timed out after {Timeout} without a result",
                deployment.Id,
                options.DeploymentTimeout);
        }
    }
}
