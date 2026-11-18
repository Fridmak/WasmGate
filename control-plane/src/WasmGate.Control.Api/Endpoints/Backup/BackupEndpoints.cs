using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Api.Endpoints.Backup;

/// <summary>
/// Downloads a consistent SQLite snapshot through the admin API.
/// </summary>
/// <remarks>
/// The snapshot is produced by <c>VACUUM INTO</c> and streamed from a temporary
/// file. The file is deleted when the response completes, so no backup data is
/// retained in the control-plane container.
/// </remarks>
internal static class BackupEndpoints
{
    /// <summary>Maps the backup route.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapBackupEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/backup", DownloadAsync);
        return app;
    }

    /// <summary>Creates and streams a snapshot of the live database.</summary>
    /// <param name="options">Control-plane options with the database path.</param>
    /// <param name="cancellationToken">Cancels the backup.</param>
    /// <returns>A downloadable SQLite file.</returns>
    private static async Task<IResult> DownloadAsync(
        ControlPlaneOptions options,
        CancellationToken cancellationToken)
    {
        var temporary = Path.Combine(
            Path.GetTempPath(),
            $"wasmgate-control-{Guid.NewGuid():N}.db");
        try
        {
            await SqliteBackup.BackupAsync(options.SqlitePath, temporary, cancellationToken);
            var stream = new FileStream(
                temporary,
                FileMode.Open,
                FileAccess.Read,
                FileShare.Read,
                bufferSize: 64 * 1024,
                FileOptions.Asynchronous | FileOptions.DeleteOnClose);
            return Results.Stream(
                stream,
                "application/octet-stream",
                $"wasmgate-control-{DateTime.UtcNow:yyyyMMdd-HHmmss}.db");
        }
        catch
        {
            TryDelete(temporary);
            throw;
        }
    }

    private static void TryDelete(string path)
    {
        try
        {
            File.Delete(path);
        }
        catch (IOException)
        {
            // The response path owns cleanup once the stream is opened.
        }
    }
}
