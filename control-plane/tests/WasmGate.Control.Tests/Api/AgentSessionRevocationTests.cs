using System.Collections.Concurrent;
using Grpc.Core;
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
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>Tests the serialized live-session lifecycle.</summary>
public sealed class AgentSessionRevocationTests
{
    [Fact]
    public async Task ClosingRegisteredSessionStopsLiveGrpcStreamAndWrites()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var operationLock = new AgentOperationLock();
        var service = new AgentControlService(
            registry,
            new SqliteAgentRepository(db.Connections),
            new SqliteAgentCertificateRepository(db.Connections),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            operationLock,
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
    public async Task OldSessionCleanupDoesNotMarkTheNewSessionOffline()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agents = new SqliteAgentRepository(db.Connections);
        var service = new AgentControlService(
            registry,
            agents,
            new SqliteAgentCertificateRepository(db.Connections),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            new AgentOperationLock(),
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var agentId = Guid.NewGuid();

        var firstTask = service.Session(
            new BlockingAgentStream(agentId),
            new TestResponseStream(),
            new TestServerCallContext());
        await registry.WaitForSessionAsync(agentId);

        var secondTask = service.Session(
            new BlockingAgentStream(agentId),
            new TestResponseStream(),
            new TestServerCallContext());
        var second = await registry.WaitForSecondSessionAsync(agentId);

        // The takeover closes the first session; its cleanup must not overwrite
        // the Online status persisted by the new session.
        await firstTask.WaitAsync(TimeSpan.FromSeconds(5));
        var stored = await agents.FindAsync(agentId, CancellationToken.None);
        Assert.NotNull(stored);
        Assert.Equal(AgentStatus.Online, stored.Status);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(second, current);

        await second.CloseAsync(CancellationToken.None);
        await secondTask.WaitAsync(TimeSpan.FromSeconds(5));
        stored = await agents.FindAsync(agentId, CancellationToken.None);
        Assert.NotNull(stored);
        Assert.Equal(AgentStatus.Offline, stored.Status);
    }

    [Fact]
    public async Task NewSessionWaitsForOldCleanupAndStaysOnline()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agents = new BlockingAgentRepository(
            new SqliteAgentRepository(db.Connections));
        var operations = new SignalingOperationLock(new AgentOperationLock());
        var service = new AgentControlService(
            registry,
            agents,
            new SqliteAgentCertificateRepository(db.Connections),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            operations,
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);
        var agentId = Guid.NewGuid();

        var firstTask = service.Session(
            new BlockingAgentStream(agentId),
            new TestResponseStream(),
            new TestServerCallContext());
        var first = await registry.WaitForSessionAsync(agentId);

        // Hold the old session's MarkOffline lookup under the agent lock, then
        // start a new Session. The new registration must wait, not be marked
        // Offline by the old cleanup.
        agents.BlockNextFind();
        await first.CloseAsync(CancellationToken.None);
        await agents.FindEntered.WaitAsync(TimeSpan.FromSeconds(5));

        var secondTask = service.Session(
            new BlockingAgentStream(agentId),
            new TestResponseStream(),
            new TestServerCallContext());
        await operations.ThirdAcquireStarted.WaitAsync(TimeSpan.FromSeconds(5));
        Assert.False(secondTask.IsCompleted);
        Assert.False(registry.TryGet(agentId, out _));

        agents.ReleaseFind();
        var second = await registry.WaitForSecondSessionAsync(agentId);
        var stored = await agents.FindAsync(agentId, CancellationToken.None);
        Assert.NotNull(stored);
        Assert.Equal(AgentStatus.Online, stored.Status);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(second, current);

        await firstTask.WaitAsync(TimeSpan.FromSeconds(5));
        await second.CloseAsync(CancellationToken.None);
        await secondTask.WaitAsync(TimeSpan.FromSeconds(5));
        stored = await agents.FindAsync(agentId, CancellationToken.None);
        Assert.NotNull(stored);
        Assert.Equal(AgentStatus.Offline, stored.Status);
    }

    [Fact]
    public async Task PersistFailureOccursBeforeRegistrationAndLeavesExistingSessionUntouched()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var registry = new TrackingSessionRegistry();
        var agentId = Guid.NewGuid();
        var existing = new TestSession(agentId);
        registry.Register(agentId, existing);

        var service = new AgentControlService(
            registry,
            new FailingAgentRepository(),
            new SqliteAgentCertificateRepository(db.Connections),
            new EmptyDeploymentRepository(),
            new SqliteAuditLog(db.Connections),
            new AgentOperationLock(),
            new ControlPlaneOptions(),
            NullLogger<AgentControlService>.Instance);

        var requestStream = new BlockingAgentStream(agentId);
        await Assert.ThrowsAsync<InvalidOperationException>(
            () => service.Session(requestStream, new TestResponseStream(), new TestServerCallContext()));

        Assert.False(existing.Closed);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(existing, current);
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

    private sealed class TrackingSessionRegistry : IAgentSessionRegistry
    {
        private readonly ConcurrentDictionary<Guid, IAgentSession> _sessions = new();
        private readonly TaskCompletionSource<IAgentSession> _registered =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource<IAgentSession> _secondRegistered =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _registrationCount;

        public int Count => _sessions.Count;

        public IAgentSession? Register(Guid agentId, IAgentSession session)
        {
            _sessions.TryGetValue(agentId, out var displaced);
            _sessions[agentId] = session;
            _registered.TrySetResult(session);
            if (Interlocked.Increment(ref _registrationCount) == 2)
            {
                _secondRegistered.TrySetResult(session);
            }

            return displaced;
        }

        public void Unregister(Guid agentId, IAgentSession session)
        {
            if (_sessions.TryGetValue(agentId, out var current) && ReferenceEquals(current, session))
            {
                _sessions.TryRemove(agentId, out _);
            }
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

        public async Task<IAgentSession> WaitForSecondSessionAsync(Guid agentId)
        {
            var session = await _secondRegistered.Task.WaitAsync(TimeSpan.FromSeconds(5));
            Assert.Equal(agentId, session.AgentId);
            return session;
        }
    }

    private sealed class SignalingOperationLock(IAgentOperationLock inner) : IAgentOperationLock
    {
        private readonly TaskCompletionSource _thirdAcquireStarted =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _acquireCalls;

        public Task ThirdAcquireStarted => _thirdAcquireStarted.Task;

        public async Task<IAsyncDisposable> AcquireAsync(
            Guid agentId,
            CancellationToken cancellationToken)
        {
            if (Interlocked.Increment(ref _acquireCalls) == 3)
            {
                _thirdAcquireStarted.TrySetResult();
            }

            return await inner.AcquireAsync(agentId, cancellationToken);
        }
    }

    private sealed class BlockingAgentRepository(IAgentRepository inner) : IAgentRepository
    {
        private readonly TaskCompletionSource _findEntered =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource _releaseFind =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _blockFind;

        public Task FindEntered => _findEntered.Task;

        public void BlockNextFind() => Interlocked.Exchange(ref _blockFind, 1);

        public void ReleaseFind() => _releaseFind.TrySetResult();

        public async Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken)
        {
            if (Interlocked.Exchange(ref _blockFind, 0) == 1)
            {
                _findEntered.TrySetResult();
                await _releaseFind.Task.WaitAsync(TimeSpan.FromSeconds(5));
            }

            return await inner.FindAsync(id, cancellationToken);
        }

        public Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken) =>
            inner.ListAsync(cancellationToken);

        public Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken) =>
            inner.FindByNameAsync(name, cancellationToken);

        public Task<bool> UpsertAsync(Agent agent, CancellationToken cancellationToken) =>
            inner.UpsertAsync(agent, cancellationToken);

        public Task SetEnvironmentAsync(
            Guid id,
            string environment,
            CancellationToken cancellationToken) =>
            inner.SetEnvironmentAsync(id, environment, cancellationToken);

        public Task<bool> DeleteAsync(Guid id, CancellationToken cancellationToken) =>
            inner.DeleteAsync(id, cancellationToken);

        public Task TouchAsync(
            Guid id,
            DateTimeOffset seenAt,
            string? activeReleaseId,
            string? activeConfigSha256,
            CancellationToken cancellationToken) =>
            inner.TouchAsync(id, seenAt, activeReleaseId, activeConfigSha256, cancellationToken);

        public Task RecordEnrollmentAsync(
            Guid id,
            DateTimeOffset seenAt,
            CancellationToken cancellationToken) =>
            inner.RecordEnrollmentAsync(id, seenAt, cancellationToken);
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

        public Task SetEnvironmentAsync(Guid id, string environment, CancellationToken cancellationToken) =>
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

        public Task RecordEnrollmentAsync(Guid id, DateTimeOffset seenAt, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RecordEnrollmentAsync");
    }

    private sealed class EmptyDeploymentRepository : IDeploymentRepository
    {
        public Task AddAsync(Deployment deployment, CancellationToken cancellationToken) => Task.CompletedTask;

        public Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken) =>
            Task.FromResult<Deployment?>(null);

        public Task<IReadOnlyList<Deployment>> ListActiveAsync(string environment, CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task<IReadOnlyList<Deployment>> ListAsync(string environment, int limit, CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task<IReadOnlyList<Deployment>> ListStaleRunningAsync(DateTimeOffset createdBefore, CancellationToken cancellationToken) =>
            Task.FromResult<IReadOnlyList<Deployment>>([]);

        public Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken) => Task.CompletedTask;
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
        protected override Task WriteResponseHeadersAsyncCore(Metadata responseHeaders) => Task.CompletedTask;
        protected override ContextPropagationToken CreatePropagationTokenCore(ContextPropagationOptions? options) => null!;
    }
}
