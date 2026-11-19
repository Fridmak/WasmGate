using System.Collections.Concurrent;
using Grpc.Core;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Tests.Storage;
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Proves that closing the registered session actually terminates the live
/// AgentControl stream and prevents further server-to-agent writes.
/// </summary>
public sealed class AgentSessionRevocationTests
{
    [Fact]
    public async Task ClosingRegisteredSessionStopsLiveGrpcStreamAndWrites()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var audit = new SqliteAuditLog(db.Connections);
        var service = new AgentControlService(
            registry,
            agents,
            certificates,
            new EmptyDeploymentRepository(),
            audit,
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var agentId = Guid.NewGuid();
        var requestStream = new BlockingAgentStream(agentId);
        var responseStream = new TestResponseStream();
        var context = new TestServerCallContext();

        var sessionTask = service.Session(requestStream, responseStream, context);
        var liveSession = await registry.WaitForSessionAsync(agentId);

        await liveSession.CloseAsync(CancellationToken.None);

        var completed = await Task.WhenAny(sessionTask, Task.Delay(TimeSpan.FromSeconds(5)));
        Assert.Same(sessionTask, completed);
        await sessionTask;
        Assert.False(registry.TryGet(agentId, out _));
        await Assert.ThrowsAnyAsync<OperationCanceledException>(
            () => liveSession.SendAsync(
                new AgentCommand(AgentCommandKind.Ping),
                CancellationToken.None).AsTask());
    }

    [Fact]
    public async Task RevocationBetweenInitialCheckAndRegistrationRejectsSession()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agents = new SqliteAgentRepository(db.Connections);
        var certificates = new SqliteAgentCertificateRepository(db.Connections);
        var service = new AgentControlService(
            registry,
            agents,
            certificates,
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var agentId = Guid.NewGuid();
        var displaced = new TestSession(agentId);
        Assert.Null(registry.Register(agentId, displaced));

        var session = new GrpcAgentSession(
            agentId,
            new TestResponseStream(),
            new TestServerCallContext(),
            registry);
        var error = await Assert.ThrowsAsync<RpcException>(() => service.RegisterSessionAsync(
            session,
            new Hello
            {
                AgentId = agentId.ToString("D"),
                Name = "race-agent",
                Version = "test",
            },
            certificateThumbprint: "missing-certificate",
            CancellationToken.None));

        Assert.Equal(StatusCode.Unauthenticated, error.StatusCode);
        Assert.False(registry.TryGet(agentId, out _));
        Assert.True(displaced.Closed);
    }

    [Fact]
    public async Task PersistHelloFailureDoesNotCloseDisplacedSession()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agentId = Guid.NewGuid();
        var displaced = new TestSession(agentId);
        Assert.Null(registry.Register(agentId, displaced));
        var service = new AgentControlService(
            registry,
            new FailingAgentRepository(),
            new SqliteAgentCertificateRepository(db.Connections),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var session = new GrpcAgentSession(
            agentId,
            new TestResponseStream(),
            new TestServerCallContext(),
            registry);

        await Assert.ThrowsAsync<InvalidOperationException>(() => service.RegisterSessionAsync(
            session,
            new Hello
            {
                AgentId = agentId.ToString("D"),
                Name = "failing-hello",
                Version = "test",
            },
            certificateThumbprint: null,
            CancellationToken.None));

        Assert.False(displaced.Closed);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(displaced, current);
    }

    [Fact]
    public async Task CancellationAfterRegistrationDoesNotCloseDisplacedSession()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agentId = Guid.NewGuid();
        var displaced = new TestSession(agentId);
        Assert.Null(registry.Register(agentId, displaced));
        var service = new AgentControlService(
            registry,
            new SqliteAgentRepository(db.Connections),
            new CanceledCertificateRepository(),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var session = new GrpcAgentSession(
            agentId,
            new TestResponseStream(),
            new TestServerCallContext(),
            registry);

        await Assert.ThrowsAsync<OperationCanceledException>(() => service.RegisterSessionAsync(
            session,
            new Hello
            {
                AgentId = agentId.ToString("D"),
                Name = "canceled-registration",
                Version = "test",
            },
            certificateThumbprint: new string('a', 64),
            CancellationToken.None));

        Assert.False(displaced.Closed);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(displaced, current);
    }

    private sealed class TestSession(Guid agentId) : IAgentSession
    {
        public Guid AgentId { get; } = agentId;

        public bool Closed { get; private set; }

        public ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken)
        {
            Closed = true;
            return ValueTask.CompletedTask;
        }
    }

    private sealed class BlockingAgentStream(Guid agentId) : IAsyncStreamReader<AgentMessage>
    {
        private int _readCount;

        public AgentMessage Current { get; private set; } = new();

        public async Task<bool> MoveNext(CancellationToken cancellationToken)
        {
            if (Interlocked.Increment(ref _readCount) == 1)
            {
                Current = new AgentMessage
                {
                    Hello = new Hello
                    {
                        AgentId = agentId.ToString("D"),
                        Name = "session-test-agent",
                        Version = "test",
                    },
                };
                return true;
            }

            await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken);
            return false;
        }
    }

    private sealed class TestResponseStream : IServerStreamWriter<ControlMessage>
    {
        public WriteOptions? WriteOptions { get; set; }

        public Task WriteAsync(ControlMessage message) => Task.CompletedTask;
    }

    private sealed class TrackingSessionRegistry : IAgentSessionRegistry
    {
        private readonly ConcurrentDictionary<Guid, IAgentSession> _sessions = new();
        private readonly TaskCompletionSource<IAgentSession> _registered =
            new(TaskCreationOptions.RunContinuationsAsynchronously);

        public int Count => _sessions.Count;

        public IAgentSession? Register(Guid agentId, IAgentSession session)
        {
            _sessions.TryGetValue(agentId, out var displaced);
            _sessions[agentId] = session;
            _registered.TrySetResult(session);
            return displaced;
        }

        public void Unregister(Guid agentId, IAgentSession session)
        {
            if (_sessions.TryGetValue(agentId, out var current) && ReferenceEquals(current, session))
            {
                _sessions.TryRemove(agentId, out _);
            }
        }

        public void Retire(IAgentSession session) => Unregister(session.AgentId, session);

        public bool TryRestore(
            Guid agentId,
            IAgentSession failedSession,
            IAgentSession displacedSession)
        {
            if (_sessions.TryGetValue(agentId, out var current) &&
                ReferenceEquals(current, failedSession))
            {
                _sessions[agentId] = displacedSession;
                return true;
            }

            return false;
        }

        public bool TryGet(
            Guid agentId,
            [System.Diagnostics.CodeAnalysis.NotNullWhen(true)] out IAgentSession? session) =>
            _sessions.TryGetValue(agentId, out session);

        public async Task<IAgentSession> WaitForSessionAsync(Guid agentId)
        {
            var session = await _registered.Task.WaitAsync(TimeSpan.FromSeconds(5));
            Assert.Equal(agentId, session.AgentId);
            return session;
        }
    }

    private sealed class FailingAgentRepository : IAgentRepository
    {
        public Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected ListAsync");

        public Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken) =>
            Task.FromResult<Agent?>(null);

        public Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken) =>
            Task.FromResult<Agent?>(null);

        public Task<bool> UpsertAsync(Agent agent, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("database unavailable");

        public Task SetEnvironmentAsync(
            Guid id,
            string environment,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected SetEnvironmentAsync");

        public Task<bool> DeleteAsync(Guid id, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected DeleteAsync");

        public Task TouchAsync(
            Guid id,
            DateTimeOffset seenAt,
            string? activeReleaseId,
            string? activeConfigSha256,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected TouchAsync");

        public Task RecordEnrollmentAsync(
            Guid id,
            DateTimeOffset seenAt,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RecordEnrollmentAsync");
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

    private sealed class EmptyDeploymentRepository : IDeploymentRepository
    {
        public Task AddAsync(Deployment deployment, CancellationToken cancellationToken) =>
            Task.CompletedTask;

        public Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken) =>
            Task.FromResult<Deployment?>(null);

        public Task<IReadOnlyList<Deployment>> ListActiveAsync(
            string environment,
            CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task<IReadOnlyList<Deployment>> ListAsync(
            string environment,
            int limit,
            CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task<IReadOnlyList<Deployment>> ListStaleRunningAsync(
            DateTimeOffset createdBefore,
            CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken) =>
            Task.CompletedTask;
    }

    private sealed class TestServerCallContext : ServerCallContext
    {
        protected override string MethodCore => "test/Session";

        protected override string HostCore => "localhost";

        protected override string PeerCore => "ipv4:127.0.0.1:12345";

        protected override DateTime DeadlineCore => DateTime.UtcNow.AddMinutes(1);

        protected override Metadata RequestHeadersCore => new();

        protected override CancellationToken CancellationTokenCore => CancellationToken.None;

        protected override Metadata ResponseTrailersCore => new();

        protected override Status StatusCore { get; set; }

        protected override WriteOptions? WriteOptionsCore { get; set; } = new();

        protected override AuthContext AuthContextCore => new(null, new Dictionary<string, List<AuthProperty>>());

        protected override Task WriteResponseHeadersAsyncCore(Metadata responseHeaders) =>
            Task.CompletedTask;

        protected override ContextPropagationToken CreatePropagationTokenCore(
            ContextPropagationOptions? options) =>
            null!;
    }
}
