using System.Net;
using System.Net.Http.Headers;
using System.Text;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Mvc.Testing;
using Microsoft.Extensions.DependencyInjection;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// End-to-end test of the authenticated backup endpoint and its workspace
/// cleanup. The factory runs the real composition root in local mode so no
/// certificates or enrollment are needed.
/// </summary>
public sealed class BackupEndpointTests
{
    [Fact]
    public async Task DownloadReturnsSnapshotAndRemovesWorkspace()
    {
        using var factory = new BackupApplicationFactory();
        using var client = factory.CreateAuthenticatedClient();
        using var response = await client.PostAsync("/api/v1/backup", content: null);

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        Assert.Equal("application/octet-stream", response.Content.Headers.ContentType?.MediaType);

        var bytes = await response.Content.ReadAsByteArrayAsync();
        Assert.True(bytes.Length > 16);
        Assert.Equal("SQLite format 3\0", Encoding.ASCII.GetString(bytes, 0, 16));

        response.Dispose();
        await WaitForEmptyWorkspaceAsync(factory.BackupRoot);
    }

    [Fact]
    public async Task UnauthenticatedRequestIsRejected()
    {
        using var factory = new BackupApplicationFactory();
        using var client = factory.CreateUnauthenticatedClient();

        using var response = await client.PostAsync("/api/v1/backup", content: null);

        Assert.Equal(HttpStatusCode.Unauthorized, response.StatusCode);
    }

    [Fact]
    public async Task ConcurrentBackupIsRejectedWithTooManyRequests()
    {
        using var factory = new BackupApplicationFactory();
        var coordinator = factory.Services.GetRequiredService<SqliteBackupCoordinator>();
        using var lease = coordinator.TryAcquire();
        Assert.NotNull(lease);

        using var client = factory.CreateAuthenticatedClient();
        using var response = await client.PostAsync("/api/v1/backup", content: null);

        Assert.Equal(HttpStatusCode.TooManyRequests, response.StatusCode);
    }

    private static async Task WaitForEmptyWorkspaceAsync(string root)
    {
        for (var attempt = 0; attempt < 50; attempt++)
        {
            if (!Directory.Exists(root) || Directory.GetDirectories(root).Length == 0)
            {
                return;
            }

            await Task.Delay(10);
        }

        Assert.Empty(Directory.GetDirectories(root));
    }

    /// <summary>
    /// Hosts <see cref="Program"/> with a unique data directory and a private
    /// backup root. Environment variables are restored immediately after the
    /// host is built, so the test does not leak configuration into other tests.
    /// </summary>
    private sealed class BackupApplicationFactory : WebApplicationFactory<Program>
    {
        private const string AdminToken = "backup-test-token";

        private readonly Dictionary<string, string?> _previous = new(StringComparer.Ordinal);

        public BackupApplicationFactory()
        {
            DataRoot = Path.Combine(
                Path.GetTempPath(),
                "wasmgate-http-backup-tests",
                Guid.NewGuid().ToString("N"));
            BackupRoot = Path.Combine(DataRoot, "backups");
            Directory.CreateDirectory(DataRoot);

            Set("WASMGATE_CP_LOCAL", "true");
            Set("WASMGATE_CP_SQLITE_PATH", Path.Combine(DataRoot, "control.db"));
            Set("WASMGATE_CP_HTTP_LISTEN", "http://127.0.0.1:19097");
            Set("WASMGATE_CP_GRPC_LISTEN", "http://127.0.0.1:19096");
            Set("WASMGATE_CP_ADMIN_TOKEN", AdminToken);
            Set("WASMGATE_CP_ARTIFACT_ROOT", Path.Combine(DataRoot, "artifacts"));
            Set("WASMGATE_CP_BACKUP_TMP", BackupRoot);
            Set("WASMGATE_CP_SIGNING_KEY", Path.Combine(DataRoot, "keys", "release-signing.key"));
            Set("WASMGATE_CP_AGENT_CA_CERT", Path.Combine(DataRoot, "ca", "agents-ca.crt"));
            Set("WASMGATE_CP_AGENT_CA_KEY", Path.Combine(DataRoot, "ca", "agents-ca.key"));
        }

        public string DataRoot { get; }

        public string BackupRoot { get; }

        public HttpClient CreateAuthenticatedClient()
        {
            var client = CreateClientAndRestore();
            client.DefaultRequestHeaders.Authorization =
                new AuthenticationHeaderValue("Bearer", AdminToken);
            return client;
        }

        public HttpClient CreateUnauthenticatedClient() => CreateClientAndRestore();

        private HttpClient CreateClientAndRestore()
        {
            try
            {
                return base.CreateClient();
            }
            finally
            {
                RestoreEnvironment();
            }
        }

        private void Set(string name, string value)
        {
            _previous[name] = Environment.GetEnvironmentVariable(name);
            Environment.SetEnvironmentVariable(name, value);
        }

        private void RestoreEnvironment()
        {
            foreach (var (name, value) in _previous)
            {
                Environment.SetEnvironmentVariable(name, value);
            }

            _previous.Clear();
        }

        protected override void Dispose(bool disposing)
        {
            base.Dispose(disposing);

            if (disposing && Directory.Exists(DataRoot))
            {
                try
                {
                    Directory.Delete(DataRoot, recursive: true);
                }
                catch (IOException)
                {
                    // A failed test cleanup must not mask the assertion result.
                }
            }
        }
    }
}
