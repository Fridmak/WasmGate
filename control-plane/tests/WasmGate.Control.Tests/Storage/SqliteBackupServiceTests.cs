using System.Text;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Tests.Storage;

/// <summary>
/// Exercises snapshot ownership: the download stream is the only owner of the
/// workspace and the single backup slot.
/// </summary>
public sealed class SqliteBackupServiceTests
{
    [Fact]
    public async Task DownloadReturnsReadableSnapshotAndCleansWorkspace()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var root = NewRoot();
        try
        {
            var service = NewService(db, root);

            var download = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(download);

            var header = new byte[16];
            await using (var stream = download!.OpenRead())
            {
                Assert.Equal(16, await stream.ReadAsync(header));
            }

            Assert.Equal("SQLite format 3\0", Encoding.ASCII.GetString(header));
            Assert.Empty(Directory.GetDirectories(root));

            using var next = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(next);
        }
        finally
        {
            DeleteRoot(root);
        }
    }

    [Fact]
    public async Task SecondBackupIsRejectedWhileTheFirstIsAlive()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var root = NewRoot();
        try
        {
            var service = NewService(db, root);

            var first = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(first);

            Assert.Null(await service.TryCreateAsync(CancellationToken.None));

            first!.Dispose();
            using var second = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(second);
        }
        finally
        {
            DeleteRoot(root);
        }
    }

    [Fact]
    public async Task CancellationReleasesTheBackupSlotAndLeavesNoWorkspace()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var root = NewRoot();
        try
        {
            var service = NewService(db, root);
            using var cancellation = new CancellationTokenSource();
            await cancellation.CancelAsync();

            await Assert.ThrowsAnyAsync<OperationCanceledException>(
                () => service.TryCreateAsync(cancellation.Token));

            Assert.Empty(Directory.GetDirectories(root));
            using var retry = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(retry);
        }
        finally
        {
            DeleteRoot(root);
        }
    }

    [Fact]
    public async Task DisposingTheStreamWithoutReadingItToEndCleansUp()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var root = NewRoot();
        try
        {
            var service = NewService(db, root);
            var download = await service.TryCreateAsync(CancellationToken.None);
            Assert.NotNull(download);

            var stream = download!.OpenRead();
            var buffer = new byte[16];
            Assert.Equal(16, await stream.ReadAsync(buffer));
            await stream.DisposeAsync();

            Assert.Empty(Directory.GetDirectories(root));
        }
        finally
        {
            DeleteRoot(root);
        }
    }

    [Fact]
    public async Task ExistingDestinationIsRefused()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var destination = Path.Combine(
            Path.GetDirectoryName(db.DatabasePath)!,
            "existing-backup.db");
        await File.WriteAllTextAsync(destination, "already here");

        await Assert.ThrowsAsync<IOException>(
            () => SqliteBackup.BackupAsync(
                db.DatabasePath,
                destination,
                CancellationToken.None));
    }

    private static SqliteBackupService NewService(SqliteTestDatabase db, string root) =>
        new(
            new ControlPlaneOptions
            {
                SqlitePath = db.DatabasePath,
                BackupTempDirectory = root,
            },
            new SqliteBackupCoordinator());

    private static string NewRoot() =>
        Path.Combine(
            Path.GetTempPath(),
            "wasmgate-backup-service-tests",
            Guid.NewGuid().ToString("N"));

    private static void DeleteRoot(string root)
    {
        if (Directory.Exists(root))
        {
            Directory.Delete(root, recursive: true);
        }
    }
}
