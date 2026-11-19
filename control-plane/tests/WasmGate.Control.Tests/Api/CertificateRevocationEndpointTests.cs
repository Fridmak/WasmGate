using System.Net;
using System.Net.Http.Headers;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Mvc.Testing;
using Microsoft.Extensions.DependencyInjection;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Repositories;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Exercises certificate revocation through the real admin HTTP endpoint and
/// the real SQLite repositories, rather than only testing the repository method.
/// </summary>
[Collection("Control-plane host")]
public sealed class CertificateRevocationEndpointTests
{
    [Fact]
    public async Task RevokeClosesLiveSessionAndBlocksEveryLaterActiveCheck()
    {
        using var factory = new CertificateApplicationFactory();
        using var client = factory.CreateAuthenticatedClient();
        var agentId = Guid.NewGuid();
        const string serial = "endpoint-cert";
        var agent = new Agent
        {
            Id = agentId,
            Name = "endpoint-agent",
            Environment = "prod",
            Version = "test",
            RegisteredAt = DateTimeOffset.UtcNow,
            LastSeenAt = DateTimeOffset.UtcNow,
        };

        var agents = factory.Services.GetRequiredService<IAgentRepository>();
        var certificates = factory.Services.GetRequiredService<IAgentCertificateRepository>();
        var sessions = factory.Services.GetRequiredService<IAgentSessionRegistry>();
        var audit = factory.Services.GetRequiredService<IAuditLog>();
        await agents.UpsertAsync(agent, CancellationToken.None);
        await certificates.AddAsync(new AgentCertificate
        {
            AgentId = agentId,
            SerialNumber = serial,
            Thumbprint = new string('a', 64),
            Pem = "test-pem",
            NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
            NotAfter = DateTimeOffset.UtcNow.AddHours(1),
        }, CancellationToken.None);

        var session = new TestSession(
            agentId,
            () => sessions.TryGet(agentId, out _));
        Assert.Null(sessions.Register(agentId, session));

        var path = $"/api/v1/agents/{agentId:D}/certificates/{serial}/revoke";
        using var response = await client.PostAsync(path, content: null);

        Assert.Equal(HttpStatusCode.NoContent, response.StatusCode);
        Assert.True(session.Closed);
        Assert.False(session.RegisteredOnClose);
        Assert.False(sessions.TryGet(agentId, out _));
        Assert.False(await certificates.IsActiveAsync(
            agentId,
            new string('a', 64),
            CancellationToken.None));

        // The same revoked identity cannot pass the active-certificate check
        // used by Session, Renew and Download.
        Assert.Empty(await certificates.ListActiveByAgentAsync(agentId, CancellationToken.None));
        var events = await audit.ListAsync(20, CancellationToken.None);
        Assert.Contains(events, auditEvent =>
            auditEvent.Action == "certificate.revoke" &&
            auditEvent.Target == $"{agentId:D}/{serial}");

        using var repeated = await client.PostAsync(path, content: null);
        Assert.Equal(HttpStatusCode.NotFound, repeated.StatusCode);

        using var unknown = await client.PostAsync(
            $"/api/v1/agents/{agentId:D}/certificates/unknown/revoke",
            content: null);
        Assert.Equal(HttpStatusCode.NotFound, unknown.StatusCode);
    }

    private sealed class TestSession(Guid agentId, Func<bool>? isRegistered = null) : IAgentSession
    {
        public Guid AgentId { get; } = agentId;

        public bool Closed { get; private set; }

        public bool RegisteredOnClose { get; private set; }

        public ValueTask SendAsync(
            WasmGate.Control.Core.Entities.Delivery.AgentCommand command,
            CancellationToken cancellationToken) => ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken)
        {
            RegisteredOnClose = isRegistered?.Invoke() ?? false;
            Closed = true;
            return ValueTask.CompletedTask;
        }
    }

    /// <summary>Hosts the actual control-plane composition root on a temp database.</summary>
    private sealed class CertificateApplicationFactory : WebApplicationFactory<Program>
    {
        private const string AdminToken = "certificate-test-token";

        private readonly Dictionary<string, string?> _previous = new(StringComparer.Ordinal);

        public CertificateApplicationFactory()
        {
            DataRoot = Path.Combine(
                Path.GetTempPath(),
                "wasmgate-http-certificate-tests",
                Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(DataRoot);

            Set("WASMGATE_CP_LOCAL", "true");
            Set("WASMGATE_CP_SQLITE_PATH", Path.Combine(DataRoot, "control.db"));
            Set("WASMGATE_CP_HTTP_LISTEN", "http://127.0.0.1:19097");
            Set("WASMGATE_CP_GRPC_LISTEN", "http://127.0.0.1:19096");
            Set("WASMGATE_CP_ADMIN_TOKEN", AdminToken);
            Set("WASMGATE_CP_ARTIFACT_ROOT", Path.Combine(DataRoot, "artifacts"));
            Set("WASMGATE_CP_SIGNING_KEY", Path.Combine(DataRoot, "keys", "release-signing.key"));
            Set("WASMGATE_CP_AGENT_CA_CERT", Path.Combine(DataRoot, "ca", "agents-ca.crt"));
            Set("WASMGATE_CP_AGENT_CA_KEY", Path.Combine(DataRoot, "ca", "agents-ca.key"));
        }

        public string DataRoot { get; }

        public HttpClient CreateAuthenticatedClient()
        {
            try
            {
                var client = CreateClient();
                client.DefaultRequestHeaders.Authorization =
                    new AuthenticationHeaderValue("Bearer", AdminToken);
                return client;
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
                    // Do not hide an assertion failure behind temp cleanup.
                }
            }
        }
    }
}
