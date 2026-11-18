using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Creates downloadable SQLite snapshots for the admin API.
/// </summary>
/// <remarks>
/// The service owns the cross-cutting policy around a snapshot: at most one
/// backup runs at a time, every snapshot lives in a private workspace, and the
/// workspace is removed when the download is disposed even if the client
/// disconnects.
/// </remarks>
public sealed class SqliteBackupService(
    ControlPlaneOptions options,
    SqliteBackupCoordinator coordinator)
{
    /// <summary>Starts one snapshot when no other backup is running.</summary>
    /// <param name="cancellationToken">Cancels the snapshot creation.</param>
    /// <returns>
    /// A download that owns the snapshot, or null when another backup already
    /// holds the single backup slot.
    /// </returns>
    public async Task<SqliteBackupDownload?> TryCreateAsync(
        CancellationToken cancellationToken)
    {
        var lease = coordinator.TryAcquire();
        if (lease is null)
        {
            return null;
        }

        SqliteBackupWorkspace? workspace = null;
        try
        {
            workspace = SqliteBackupWorkspace.Create(options.BackupTempDirectory);
            var path = workspace.PathFor("control.db");
            await SqliteBackup.BackupAsync(options.SqlitePath, path, cancellationToken);
            return new SqliteBackupDownload(path, workspace, lease);
        }
        catch
        {
            workspace?.Dispose();
            lease.Dispose();
            throw;
        }
    }
}
