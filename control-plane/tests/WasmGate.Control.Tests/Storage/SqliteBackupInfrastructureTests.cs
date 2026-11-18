using Microsoft.Data.Sqlite;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Tests.Storage;

/// <summary>
/// Verifies the private workspace, the backup gate and Unix file modes that
/// protect SQLite sidecar files.
/// </summary>
public sealed class SqliteBackupInfrastructureTests
{
    [Fact]
    public void WorkspaceIsPrivateAndRemovesItself()
    {
        var root = Path.Combine(
            Path.GetTempPath(),
            "wasmgate-backup-workspace-tests",
            Guid.NewGuid().ToString("N"));

        try
        {
            var workspace = SqliteBackupWorkspace.Create(root);
            Assert.True(Directory.Exists(workspace.DirectoryPath));

            if (!OperatingSystem.IsWindows())
            {
                Assert.Equal(
                    UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute,
                    File.GetUnixFileMode(workspace.DirectoryPath));
            }

            workspace.Dispose();
            Assert.False(Directory.Exists(workspace.DirectoryPath));
        }
        finally
        {
            if (Directory.Exists(root))
            {
                Directory.Delete(root, recursive: true);
            }
        }
    }

    [Fact]
    public void CleanStaleRemovesOnlyOldWorkspaces()
    {
        var root = Path.Combine(
            Path.GetTempPath(),
            "wasmgate-backup-cleanup-tests",
            Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(root);
        try
        {
            var oldWorkspace = Path.Combine(root, "wasmgate-control-backup-old");
            var freshWorkspace = Path.Combine(root, "wasmgate-control-backup-fresh");
            Directory.CreateDirectory(oldWorkspace);
            Directory.CreateDirectory(freshWorkspace);
            Directory.SetLastWriteTimeUtc(
                oldWorkspace,
                DateTime.UtcNow.AddHours(-48));

            SqliteBackupWorkspace.CleanStale(root, TimeSpan.FromHours(24), NullLogger.Instance);

            Assert.False(Directory.Exists(oldWorkspace));
            Assert.True(Directory.Exists(freshWorkspace));
        }
        finally
        {
            Directory.Delete(root, recursive: true);
        }
    }

    [Fact]
    public void CoordinatorAllowsOneLeaseAtATime()
    {
        var coordinator = new SqliteBackupCoordinator();

        var first = coordinator.TryAcquire();
        Assert.NotNull(first);
        Assert.Null(coordinator.TryAcquire());

        first!.Dispose();
        using var second = coordinator.TryAcquire();
        Assert.NotNull(second);

        first.Dispose(); // idempotent: must not release the second lease
        Assert.Null(coordinator.TryAcquire());
    }

    [Fact]
    public void FilePermissionsRestrictAnExistingFileOnUnix()
    {
        if (OperatingSystem.IsWindows())
        {
            return;
        }

        var path = Path.Combine(Path.GetTempPath(), $"wasmgate-perm-{Guid.NewGuid():N}.tmp");
        try
        {
            File.WriteAllText(path, "secret");
            File.SetUnixFileMode(
                path,
                UnixFileMode.UserRead |
                UnixFileMode.UserWrite |
                UnixFileMode.GroupRead |
                UnixFileMode.OtherRead);

            SqliteFilePermissions.RestrictToOwner(path, NullLogger.Instance);

            Assert.Equal(
                UnixFileMode.UserRead | UnixFileMode.UserWrite,
                File.GetUnixFileMode(path));
        }
        finally
        {
            File.Delete(path);
        }
    }

    [Fact]
    public void FilePermissionsIgnoreAMissingFile()
    {
        var path = Path.Combine(Path.GetTempPath(), $"wasmgate-missing-{Guid.NewGuid():N}.tmp");

        SqliteFilePermissions.RestrictToOwner(path, NullLogger.Instance);
    }

    [Fact]
    public async Task InitializerRestrictsDatabaseWalAndSharedMemoryOnUnix()
    {
        if (OperatingSystem.IsWindows())
        {
            return;
        }

        var root = Path.Combine(
            Path.GetTempPath(),
            "wasmgate-initializer-tests",
            Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(root);
        var databasePath = Path.Combine(root, "control.db");
        var options = new ControlPlaneOptions { SqlitePath = databasePath };
        var connectionString = new SqliteConnectionStringBuilder
        {
            DataSource = databasePath,
            Mode = SqliteOpenMode.ReadWriteCreate,
            Pooling = false,
            ForeignKeys = true,
        }.ToString();

        using var instanceLock = new SqliteInstanceLock();
        try
        {
            var initializer = new SqliteStorageInitializer(
                options,
                new SqliteConnectionFactory(connectionString),
                instanceLock,
                NullLogger<SqliteStorageInitializer>.Instance);

            await initializer.InitializeAsync(CancellationToken.None);

            foreach (var path in new[]
                     {
                         databasePath,
                         databasePath + "-wal",
                         databasePath + "-shm",
                         SqliteInstanceLock.PathFor(databasePath),
                     })
            {
                if (File.Exists(path))
                {
                    Assert.Equal(
                        UnixFileMode.UserRead | UnixFileMode.UserWrite,
                        File.GetUnixFileMode(path));
                }
            }
        }
        finally
        {
            Directory.Delete(root, recursive: true);
        }
    }
}
