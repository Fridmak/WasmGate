using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Api.Endpoints.Backup;

/// <summary>
/// Downloads a consistent SQLite snapshot through the admin API.
/// </summary>
/// <remarks>
/// The snapshot is produced by <c>VACUUM INTO</c> in a private temporary
/// workspace and streamed directly to the caller. The workspace is removed when
/// the response stream is disposed, so a completed download, a client
/// disconnect and a cancelled request all clean up the snapshot. A second
/// concurrent request receives 429 instead of starting another
/// <c>VACUUM INTO</c>.
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
    /// <param name="backups">Snapshot service with the database path and gate.</param>
    /// <param name="cancellationToken">Cancels the snapshot creation.</param>
    /// <returns>A downloadable SQLite file, or 429 when a backup is already running.</returns>
    private static async Task<IResult> DownloadAsync(
        SqliteBackupService backups,
        CancellationToken cancellationToken)
    {
        var download = await backups.TryCreateAsync(cancellationToken);
        if (download is null)
        {
            return Results.StatusCode(StatusCodes.Status429TooManyRequests);
        }

        Stream? stream = null;
        try
        {
            stream = download.OpenRead();
            return Results.Stream(
                stream,
                "application/octet-stream",
                $"wasmgate-control-{DateTime.UtcNow:yyyyMMdd-HHmmss}.db");
        }
        catch
        {
            stream?.Dispose();
            download.Dispose();
            throw;
        }
    }
}
