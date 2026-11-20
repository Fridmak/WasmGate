using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Tests.Storage;

namespace WasmGate.Control.Tests.Api;

/// <summary>Tests the periodic backstop for a silent revoked session.</summary>
public sealed class CertificateSessionMonitorTests
{
    [Fact]
    public async Task RevokedSilentSessionIsClosedByTheMonitor()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agent = new Agent
        {
            Id = Guid.NewGuid(),
            Name = "monitor-agent",
            Environment = "prod",
            Version = "test",
            RegisteredAt = DateTimeOffset.UtcNow,
            LastSeenAt = DateTimeOffset.UtcNow,
        };
        await agents.UpsertAsync(agent, CancellationToken.None);
        var certificate = new AgentCertificate
        {
            AgentId = agent.Id,
            SerialNumber = "monitor-cert",
            Thumbprint = new string('a', 64),
            Pem = "pem",
            NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
            NotAfter = DateTimeOffset.UtcNow.AddHours(1),
        };
        await certificates.AddAsync(certificate, CancellationToken.None);

        var options = new ControlPlaneOptions
        {
            CertificateRecheckInterval = TimeSpan.FromMilliseconds(25),
        };
        var registry = new InMemoryAgentSessionRegistry();
        var monitor = new CertificateSessionMonitor(
            certificates,
            registry,
            new AgentOperationLock(),
            options,
            NullLogger.Instance);
        var session = new TestSession(agent.Id);
        Assert.Null(registry.Register(agent.Id, session));
        using var cancellation = new CancellationTokenSource();
        var monitorTask = monitor.MonitorAsync(
            agent.Id,
            certificate.Thumbprint,
            session,
            cancellation.Token);

        await certificates.RevokeAsync(
            agent.Id,
            certificate.SerialNumber,
            CancellationToken.None);

        await session.Closed.WaitAsync(TimeSpan.FromSeconds(2));
        cancellation.Cancel();
        await monitorTask;
        Assert.True(session.IsClosed);
        Assert.False(registry.TryGet(agent.Id, out _));
    }

    [Fact]
    public async Task MonitorWaitsForTheAgentLockBeforeClosing()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var agentId = Guid.NewGuid();
        await agents.UpsertAsync(new Agent
        {
            Id = agentId,
            Name = "contended-agent",
            Environment = "prod",
            Version = "test",
            RegisteredAt = DateTimeOffset.UtcNow,
            LastSeenAt = DateTimeOffset.UtcNow,
        }, CancellationToken.None);
        var certificate = new AgentCertificate
        {
            AgentId = agentId,
            SerialNumber = "contended-cert",
            Thumbprint = new string('d', 64),
            Pem = "pem",
            NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
            NotAfter = DateTimeOffset.UtcNow.AddHours(1),
        };
        await certificates.AddAsync(certificate, CancellationToken.None);

        var registry = new InMemoryAgentSessionRegistry();
        var session = new TestSession(agentId);
        registry.Register(agentId, session);
        var innerLock = new AgentOperationLock();
        var signalingLock = new SignalingOperationLock(innerLock);
        var monitor = new CertificateSessionMonitor(
            certificates,
            registry,
            signalingLock,
            new ControlPlaneOptions { CertificateRecheckInterval = TimeSpan.FromMilliseconds(25) },
            NullLogger.Instance);
        var heldLease = await innerLock.AcquireAsync(agentId, CancellationToken.None);
        using var cancellation = new CancellationTokenSource();

        var monitorTask = monitor.MonitorAsync(
            agentId,
            certificate.Thumbprint,
            session,
            cancellation.Token);
        await signalingLock.AcquireAttempted.WaitAsync(TimeSpan.FromSeconds(5));

        await certificates.RevokeAsync(
            agentId,
            certificate.SerialNumber,
            CancellationToken.None);
        Assert.False(session.IsClosed);
        Assert.True(registry.TryGet(agentId, out _));

        await heldLease.DisposeAsync();
        await session.Closed.WaitAsync(TimeSpan.FromSeconds(5));
        cancellation.Cancel();
        await monitorTask;
        Assert.False(registry.TryGet(agentId, out _));
    }

    [Fact]
    public async Task FailedCertificateCheckClosesAndUnregistersSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var session = new TestSession(Guid.NewGuid());
        registry.Register(session.AgentId, session);
        var monitor = new CertificateSessionMonitor(
            new ThrowingCertificateRepository(),
            registry,
            new AgentOperationLock(),
            new ControlPlaneOptions { CertificateRecheckInterval = TimeSpan.FromMilliseconds(25) },
            NullLogger.Instance);
        using var cancellation = new CancellationTokenSource();

        await monitor.MonitorAsync(
            session.AgentId,
            new string('b', 64),
            session,
            cancellation.Token);

        Assert.True(session.IsClosed);
        Assert.False(registry.TryGet(session.AgentId, out _));
    }

    [Fact]
    public async Task OperationCanceledFromRepositoryWithoutSessionCancellationClosesSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var session = new TestSession(Guid.NewGuid());
        registry.Register(session.AgentId, session);
        var monitor = new CertificateSessionMonitor(
            new CanceledCertificateRepository(),
            registry,
            new AgentOperationLock(),
            new ControlPlaneOptions { CertificateRecheckInterval = TimeSpan.FromMilliseconds(25) },
            NullLogger.Instance);
        using var cancellation = new CancellationTokenSource();

        await monitor.MonitorAsync(
            session.AgentId,
            new string('c', 64),
            session,
            cancellation.Token);

        Assert.True(session.IsClosed);
        Assert.False(registry.TryGet(session.AgentId, out _));
    }

    private sealed class SignalingOperationLock(IAgentOperationLock inner) : IAgentOperationLock
    {
        private readonly TaskCompletionSource _acquireAttempted =
            new(TaskCreationOptions.RunContinuationsAsynchronously);

        public Task AcquireAttempted => _acquireAttempted.Task;

        public async Task<IAsyncDisposable> AcquireAsync(
            Guid agentId,
            CancellationToken cancellationToken)
        {
            _acquireAttempted.TrySetResult();
            return await inner.AcquireAsync(agentId, cancellationToken);
        }
    }

    private sealed class TestSession(Guid agentId) : IAgentSession
    {
        private readonly TaskCompletionSource _closed =
            new(TaskCreationOptions.RunContinuationsAsynchronously);

        public Guid AgentId { get; } = agentId;

        public Task Closed => _closed.Task;

        public bool IsClosed => _closed.Task.IsCompleted;

        public ValueTask SendAsync(
            AgentCommand command,
            CancellationToken cancellationToken) => ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken)
        {
            _closed.TrySetResult();
            return ValueTask.CompletedTask;
        }
    }

    private sealed class CanceledCertificateRepository : IAgentCertificateRepository
    {
        public Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected AddAsync");

        public Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
            Guid agentId,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected ListActiveByAgentAsync");

        public Task<bool> IsActiveAsync(
            Guid agentId,
            string thumbprint,
            CancellationToken cancellationToken) =>
            throw new OperationCanceledException("repository timeout");

        public Task<bool> RevokeAsync(
            Guid agentId,
            string serialNumber,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RevokeAsync");

        public Task<CertificateRotationResult> RotateAsync(
            AgentCertificate certificate,
            string? callerThumbprint,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RotateAsync");
    }

    private sealed class ThrowingCertificateRepository : IAgentCertificateRepository
    {
        public Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected AddAsync");

        public Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
            Guid agentId,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected ListActiveByAgentAsync");

        public Task<bool> IsActiveAsync(
            Guid agentId,
            string thumbprint,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("repository unavailable");

        public Task<bool> RevokeAsync(
            Guid agentId,
            string serialNumber,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RevokeAsync");

        public Task<CertificateRotationResult> RotateAsync(
            AgentCertificate certificate,
            string? callerThumbprint,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RotateAsync");
    }
}
