using System.Security.Cryptography;
using System.Text;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Agents.Enrollment;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Tests.Storage;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests certificate replacement and session invalidation.</summary>
public sealed class CertificateLifecycleTests
{
    [Fact]
    public async Task RenewalRevokesOldCertificateClosesSessionAndAuditsRotation()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("renew-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);

        var oldCertificate = NewCertificate(agent.Id, "old", 'a');
        await certificates.AddAsync(oldCertificate, CancellationToken.None);

        var sessions = new InMemoryAgentSessionRegistry();
        var session = new TestSession(
            agent.Id,
            () => sessions.TryGet(agent.Id, out _));
        sessions.Register(agent.Id, session);
        var audit = new TestAuditLog();
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            audit,
            new TestEnrollmentTokenRepository(),
            new TestCertificateAuthority());

        await provisioning.RenewAsync(
            agent.Id,
            "csr",
            oldCertificate.Thumbprint,
            CancellationToken.None);

        Assert.True(session.Closed);
        Assert.False(session.RegisteredOnClose);
        Assert.False(sessions.TryGet(agent.Id, out _));
        Assert.False(await certificates.IsActiveAsync(
            agent.Id,
            oldCertificate.Thumbprint,
            CancellationToken.None));
        Assert.Contains(audit.Events, auditEvent =>
            auditEvent.Action == "certificate.revoke" &&
            auditEvent.Target == $"{agent.Id:D}/old" &&
            auditEvent.DetailsJson == "{\"reason\":\"rotation\",\"replacedBy\":\"serial-1\"}");
        Assert.Contains(audit.Events, auditEvent => auditEvent.Action == "agent.renew");
    }

    [Fact]
    public async Task ReEnrollmentWithPerAgentTokenRotatesCertificateAndClosesSession()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("enroll-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);

        var oldCertificate = NewCertificate(agent.Id, "old", 'b');
        await certificates.AddAsync(oldCertificate, CancellationToken.None);

        var sessions = new InMemoryAgentSessionRegistry();
        var session = new TestSession(
            agent.Id,
            () => sessions.TryGet(agent.Id, out _));
        sessions.Register(agent.Id, session);
        var audit = new TestAuditLog();
        var tokens = new TestEnrollmentTokenRepository
        {
            Record = new EnrollmentToken
            {
                AgentId = agent.Id,
                TokenHash = new string('c', 64),
                Environment = "prod",
                MaxUses = 1,
                ExpiresAt = DateTimeOffset.UtcNow.AddMinutes(5),
            },
        };
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            audit,
            tokens,
            new TestCertificateAuthority());

        await provisioning.EnrollAsync(
            agent.Id.ToString("D"),
            name: null,
            token: "token",
            csrPem: "csr",
            CancellationToken.None);

        Assert.True(session.Closed);
        Assert.False(session.RegisteredOnClose);
        Assert.False(sessions.TryGet(agent.Id, out _));
        Assert.False(await certificates.IsActiveAsync(
            agent.Id,
            oldCertificate.Thumbprint,
            CancellationToken.None));
        Assert.Contains(audit.Events, auditEvent =>
            auditEvent.Action == "certificate.revoke" &&
            auditEvent.Target == $"{agent.Id:D}/old");
        Assert.Contains(audit.Events, auditEvent => auditEvent.Action == "agent.enroll");
    }

    [Fact]
    public async Task RevokedCallerCannotRenew()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("revoked-caller-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);
        var oldCertificate = NewCertificate(agent.Id, "old", 'c');
        await certificates.AddAsync(oldCertificate, CancellationToken.None);
        Assert.True(await certificates.RevokeAsync(
            agent.Id,
            oldCertificate.SerialNumber,
            CancellationToken.None));

        var session = new TestSession(agent.Id);
        var sessions = new InMemoryAgentSessionRegistry();
        sessions.Register(agent.Id, session);
        var authority = new TestCertificateAuthority();
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            new TestAuditLog(),
            new TestEnrollmentTokenRepository(),
            authority);

        await Assert.ThrowsAsync<EnrollmentException>(() => provisioning.RenewAsync(
            agent.Id,
            "csr",
            oldCertificate.Thumbprint,
            CancellationToken.None));

        // The caller check runs before signing and before the session is
        // touched, so a revoked caller cannot burn CA work or keep the lock.
        Assert.Equal(0, authority.IssueCalls);
        Assert.True(sessions.TryGet(agent.Id, out var current));
        Assert.Same(session, current);
        Assert.False(session.Closed);
        Assert.Empty(await certificates.ListActiveByAgentAsync(agent.Id, CancellationToken.None));
    }

    [Fact]
    public async Task ConcurrentRenewalsHaveExactlyOneWinner()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("concurrent-renew-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);
        var oldCertificate = NewCertificate(agent.Id, "old", 'd');
        await certificates.AddAsync(oldCertificate, CancellationToken.None);

        var sessions = new InMemoryAgentSessionRegistry();
        sessions.Register(agent.Id, new TestSession(agent.Id));
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            new TestAuditLog(),
            new TestEnrollmentTokenRepository(),
            new TestCertificateAuthority());

        async Task<Exception?> RenewAsync()
        {
            try
            {
                await provisioning.RenewAsync(
                    agent.Id,
                    "csr",
                    oldCertificate.Thumbprint,
                    CancellationToken.None);
                return null;
            }
            catch (Exception ex)
            {
                return ex;
            }
        }

        var outcomes = await Task.WhenAll(RenewAsync(), RenewAsync());

        Assert.Single(outcomes, outcome => outcome is null);
        Assert.IsType<EnrollmentException>(outcomes.Single(outcome => outcome is not null));
        Assert.False(sessions.TryGet(agent.Id, out _));
        var active = await certificates.ListActiveByAgentAsync(agent.Id, CancellationToken.None);
        Assert.Single(active);
        Assert.True(await certificates.IsActiveAsync(
            agent.Id,
            active[0].Thumbprint,
            CancellationToken.None));
    }

    [Fact]
    public async Task AuditFailureDoesNotFailRenewal()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("audit-failure-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);
        var oldCertificate = NewCertificate(agent.Id, "old", 'e');
        await certificates.AddAsync(oldCertificate, CancellationToken.None);

        var sessions = new InMemoryAgentSessionRegistry();
        sessions.Register(agent.Id, new TestSession(agent.Id));
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            new TestAuditLog { ThrowOnAction = "agent.renew" },
            new TestEnrollmentTokenRepository(),
            new TestCertificateAuthority());

        var result = await provisioning.RenewAsync(
            agent.Id,
            "csr",
            oldCertificate.Thumbprint,
            CancellationToken.None);

        Assert.NotEmpty(result.CertificatePem);
        Assert.False(sessions.TryGet(agent.Id, out _));
        Assert.False(await certificates.IsActiveAsync(
            agent.Id,
            oldCertificate.Thumbprint,
            CancellationToken.None));
        Assert.Single(await certificates.ListActiveByAgentAsync(agent.Id, CancellationToken.None));
    }

    [Fact]
    public async Task DeleteAgentClosesAndUnregistersSession()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = NewAgent("delete-agent");
        await agents.UpsertAsync(agent, CancellationToken.None);
        var sessions = new InMemoryAgentSessionRegistry();
        var session = new TestSession(
            agent.Id,
            () => sessions.TryGet(agent.Id, out _));
        sessions.Register(agent.Id, session);
        var provisioning = NewProvisioning(
            agents,
            certificates,
            sessions,
            new TestAuditLog(),
            new TestEnrollmentTokenRepository(),
            new TestCertificateAuthority());

        await provisioning.DeleteAgentAsync(agent.Id, "admin", CancellationToken.None);

        Assert.True(session.Closed);
        Assert.False(session.RegisteredOnClose);
        Assert.False(sessions.TryGet(agent.Id, out _));
        Assert.Null(await agents.FindAsync(agent.Id, CancellationToken.None));
    }

    private static AgentProvisioningService NewProvisioning(
        IAgentRepository agents,
        IAgentCertificateRepository certificates,
        IAgentSessionRegistry sessions,
        IAuditLog audit,
        IEnrollmentTokenRepository tokens,
        ICertificateAuthority authority) => new(
        agents,
        tokens,
        certificates,
        new TestEnvironmentRepository(),
        new TestEnvironmentOperationLock(),
        authority,
        sessions,
        new AgentOperationLock(),
        audit,
        NullLogger<AgentProvisioningService>.Instance,
        new ControlPlaneOptions
        {
            AgentCertificateLifetime = TimeSpan.FromDays(30),
        });

    private static Agent NewAgent(string name) => new()
    {
        Id = Guid.NewGuid(),
        Name = name,
        Environment = "prod",
        Version = "test",
        RegisteredAt = DateTimeOffset.UtcNow,
        LastSeenAt = DateTimeOffset.UtcNow,
    };

    private static AgentCertificate NewCertificate(Guid agentId, string serial, char thumbprint) => new()
    {
        AgentId = agentId,
        SerialNumber = serial,
        Thumbprint = new string(thumbprint, 64),
        Pem = $"pem-{serial}",
        NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
        NotAfter = DateTimeOffset.UtcNow.AddHours(1),
    };

    private sealed class TestCertificateAuthority : ICertificateAuthority
    {
        private int _serial;
        private int _issueCalls;

        public int IssueCalls => Volatile.Read(ref _issueCalls);

        public Task<string> GetCaCertificatePemAsync(CancellationToken cancellationToken) =>
            Task.FromResult("ca");

        public Task<IssuedCertificate> IssueAsync(
            string csrPem,
            string commonName,
            DateTimeOffset notAfter,
            CancellationToken cancellationToken)
        {
            Interlocked.Increment(ref _issueCalls);
            var serial = $"serial-{Interlocked.Increment(ref _serial)}";
            var thumbprint = Convert.ToHexString(
                SHA256.HashData(Encoding.UTF8.GetBytes(serial)));
            return Task.FromResult(new IssuedCertificate(
                $"pem-{serial}",
                serial,
                thumbprint,
                DateTimeOffset.UtcNow.AddMinutes(-1),
                notAfter));
        }
    }

    private sealed class TestEnrollmentTokenRepository : IEnrollmentTokenRepository
    {
        public EnrollmentToken? Record { get; init; }

        public Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken) =>
            Task.CompletedTask;

        public Task<EnrollmentToken?> FindByHashAsync(
            string tokenHash,
            CancellationToken cancellationToken) =>
            Task.FromResult(Record);

        public Task<bool> ConsumeAsync(Guid id, CancellationToken cancellationToken) =>
            Task.FromResult(Record is not null && Record.Id == id);
    }

    private sealed class TestEnvironmentRepository : IEnvironmentRepository
    {
        public Task<IReadOnlyList<string>> ListAsync(CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<string>>(["prod"]);

        public Task<bool> EnsureAsync(
            string name,
            string actor,
            CancellationToken cancellationToken) =>
            Task.FromResult(false);

        public Task DeleteAsync(string name, CancellationToken cancellationToken) =>
            Task.CompletedTask;
    }

    private sealed class TestEnvironmentOperationLock : IEnvironmentOperationLock
    {
        public Task<IAsyncDisposable> AcquireAsync(
            string environment,
            CancellationToken cancellationToken) =>
            Task.FromResult<IAsyncDisposable>(new NoopAsyncDisposable());
    }

    private sealed class NoopAsyncDisposable : IAsyncDisposable
    {
        public ValueTask DisposeAsync() => ValueTask.CompletedTask;
    }

    private sealed class TestSession(
        Guid agentId,
        Func<bool>? isRegistered = null) : IAgentSession
    {
        public Guid AgentId { get; } = agentId;

        public bool Closed { get; private set; }

        public bool RegisteredOnClose { get; private set; }

        public ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken)
        {
            RegisteredOnClose = isRegistered?.Invoke() ?? false;
            Closed = true;
            return ValueTask.CompletedTask;
        }
    }

    private sealed class TestAuditLog : IAuditLog
    {
        public List<AuditEvent> Events { get; } = [];

        public string? ThrowOnAction { get; init; }

        public Task AppendAsync(AuditEvent auditEvent, CancellationToken cancellationToken)
        {
            if (auditEvent.Action == ThrowOnAction)
            {
                throw new InvalidOperationException("audit unavailable");
            }

            Events.Add(auditEvent);
            return Task.CompletedTask;
        }

        public Task<IReadOnlyList<AuditEvent>> ListAsync(
            int limit,
            CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<AuditEvent>>(Events);
    }
}
