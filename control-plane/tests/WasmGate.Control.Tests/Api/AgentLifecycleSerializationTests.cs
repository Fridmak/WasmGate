using Grpc.Core;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Endpoints.Agents;
using WasmGate.Control.Api.Grpc;
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
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Proves that a live session, certificate revocation, renewal and deletion all
/// acquire the same per-agent lifecycle lock and clean the registry under it.
/// </summary>
public sealed class AgentLifecycleSerializationTests
{
    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);

    [Fact]
    public async Task SessionWaitsForRenewAndRenewClosesRegisteredSession()
    {
        await using var environment = await TestEnvironment.CreateAsync();
        var sessionTask = environment.StartSession();
        await environment.OperationLock.FirstLeaseHeld.WaitAsync(Timeout);
        try
        {
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            var renewTask = environment.Provisioning.RenewAsync(
                environment.Agent.Id,
                "csr",
                environment.Certificate.Thumbprint,
                CancellationToken.None);
            await environment.OperationLock.SecondAcquireStarted.WaitAsync(Timeout);
            Assert.Equal(0, environment.Authority.IssueCalls);
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            environment.OperationLock.ReleaseFirstLease();
            await renewTask.WaitAsync(Timeout);
            await sessionTask.WaitAsync(Timeout);

            Assert.Equal(1, environment.Authority.IssueCalls);
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));
            Assert.False(await environment.Certificates.IsActiveAsync(
                environment.Agent.Id,
                environment.Certificate.Thumbprint,
                CancellationToken.None));
        }
        finally
        {
            environment.OperationLock.ReleaseFirstLease();
        }
    }

    [Fact]
    public async Task SessionWaitsForEnrollAndEnrollClosesRegisteredSession()
    {
        await using var environment = await TestEnvironment.CreateAsync();
        var sessionTask = environment.StartSession();
        await environment.OperationLock.FirstLeaseHeld.WaitAsync(Timeout);
        try
        {
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            var enrollTask = environment.Provisioning.EnrollAsync(
                environment.Agent.Id.ToString("D"),
                name: null,
                token: "token",
                csrPem: "csr",
                CancellationToken.None);
            await environment.OperationLock.SecondAcquireStarted.WaitAsync(Timeout);
            Assert.Equal(0, environment.Authority.IssueCalls);
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            environment.OperationLock.ReleaseFirstLease();
            await enrollTask.WaitAsync(Timeout);
            await sessionTask.WaitAsync(Timeout);

            Assert.Equal(1, environment.Authority.IssueCalls);
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));
            Assert.False(await environment.Certificates.IsActiveAsync(
                environment.Agent.Id,
                environment.Certificate.Thumbprint,
                CancellationToken.None));
        }
        finally
        {
            environment.OperationLock.ReleaseFirstLease();
        }
    }

    [Fact]
    public async Task SessionWaitsForRevokeAndRevokeRemovesItFromTheRegistry()
    {
        await using var environment = await TestEnvironment.CreateAsync();
        var sessionTask = environment.StartSession();
        await environment.OperationLock.FirstLeaseHeld.WaitAsync(Timeout);
        try
        {
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            var revokeTask = AgentEndpoints.RevokeCertificate(
                environment.Agent.Id,
                environment.Certificate.SerialNumber,
                environment.Certificates,
                environment.Sessions,
                environment.OperationLock,
                environment.Audit,
                NullLogger<Program>.Instance,
                CancellationToken.None);
            await environment.OperationLock.SecondAcquireStarted.WaitAsync(Timeout);
            Assert.True(await environment.Certificates.IsActiveAsync(
                environment.Agent.Id,
                environment.Certificate.Thumbprint,
                CancellationToken.None));
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            environment.OperationLock.ReleaseFirstLease();
            await revokeTask.WaitAsync(Timeout);
            await sessionTask.WaitAsync(Timeout);

            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));
            Assert.False(await environment.Certificates.IsActiveAsync(
                environment.Agent.Id,
                environment.Certificate.Thumbprint,
                CancellationToken.None));
        }
        finally
        {
            environment.OperationLock.ReleaseFirstLease();
        }
    }

    [Fact]
    public async Task SessionWaitsForDeleteAndDeleteClosesIt()
    {
        await using var environment = await TestEnvironment.CreateAsync();
        var sessionTask = environment.StartSession();
        await environment.OperationLock.FirstLeaseHeld.WaitAsync(Timeout);
        try
        {
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            var deleteTask = environment.Provisioning.DeleteAgentAsync(
                environment.Agent.Id,
                "admin",
                CancellationToken.None);
            await environment.OperationLock.SecondAcquireStarted.WaitAsync(Timeout);
            Assert.NotNull(await environment.Agents.FindAsync(
                environment.Agent.Id,
                CancellationToken.None));
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));

            environment.OperationLock.ReleaseFirstLease();
            await deleteTask.WaitAsync(Timeout);
            await sessionTask.WaitAsync(Timeout);

            Assert.Null(await environment.Agents.FindAsync(
                environment.Agent.Id,
                CancellationToken.None));
            Assert.False(environment.Sessions.TryGet(environment.Agent.Id, out _));
        }
        finally
        {
            environment.OperationLock.ReleaseFirstLease();
        }
    }

    private sealed class TestEnvironment : IAsyncDisposable
    {
        private readonly SqliteTestDatabase _database;

        private TestEnvironment(
            SqliteTestDatabase database,
            Agent agent,
            AgentCertificate certificate,
            InMemoryAgentSessionRegistry sessions,
            HoldFirstOperationLock operationLock,
            TestCertificateAuthority authority,
            SqliteAgentRepository agents,
            SqliteAgentCertificateRepository certificates,
            SqliteAuditLog audit,
            AgentProvisioningService provisioning,
            AgentControlService control)
        {
            _database = database;
            Agent = agent;
            Certificate = certificate;
            Sessions = sessions;
            OperationLock = operationLock;
            Authority = authority;
            Agents = agents;
            Certificates = certificates;
            Audit = audit;
            Provisioning = provisioning;
            Control = control;
        }

        public Agent Agent { get; }

        public AgentCertificate Certificate { get; }

        public InMemoryAgentSessionRegistry Sessions { get; }

        public HoldFirstOperationLock OperationLock { get; }

        public TestCertificateAuthority Authority { get; }

        public SqliteAgentRepository Agents { get; }

        public SqliteAgentCertificateRepository Certificates { get; }

        public SqliteAuditLog Audit { get; }

        public AgentProvisioningService Provisioning { get; }

        public AgentControlService Control { get; }

        public static async Task<TestEnvironment> CreateAsync()
        {
            var database = await SqliteTestDatabase.CreateAsync();
            var agents = new SqliteAgentRepository(database.Connections);
            var certificates = new SqliteAgentCertificateRepository(database.Connections);
            var audit = new SqliteAuditLog(database.Connections);
            var agent = new Agent
            {
                Id = Guid.NewGuid(),
                Name = "serialized-agent",
                Environment = "prod",
                Version = "test",
                RegisteredAt = DateTimeOffset.UtcNow,
                LastSeenAt = DateTimeOffset.UtcNow,
            };
            await agents.UpsertAsync(agent, CancellationToken.None);
            var certificate = new AgentCertificate
            {
                AgentId = agent.Id,
                SerialNumber = "serialized-old",
                Thumbprint = new string('a', 64),
                Pem = "pem-old",
                NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
                NotAfter = DateTimeOffset.UtcNow.AddHours(1),
            };
            await certificates.AddAsync(certificate, CancellationToken.None);

            var sessions = new InMemoryAgentSessionRegistry();
            var operationLock = new HoldFirstOperationLock(new AgentOperationLock());
            var authority = new TestCertificateAuthority();
            var tokens = new TestEnrollmentTokenRepository
            {
                Record = new EnrollmentToken
                {
                    AgentId = agent.Id,
                    TokenHash = new string('a', 64),
                    Environment = "prod",
                    MaxUses = 1,
                    ExpiresAt = DateTimeOffset.UtcNow.AddMinutes(5),
                },
            };
            var provisioning = new AgentProvisioningService(
                agents,
                tokens,
                certificates,
                new TestEnvironmentRepository(),
                new TestEnvironmentOperationLock(),
                authority,
                sessions,
                operationLock,
                audit,
                NullLogger<AgentProvisioningService>.Instance,
                new ControlPlaneOptions
                {
                    AgentCertificateLifetime = TimeSpan.FromDays(30),
                });
            var control = new AgentControlService(
                sessions,
                agents,
                certificates,
                new EmptyDeploymentRepository(),
                audit,
                operationLock,
                new ControlPlaneOptions(),
                NullLogger<AgentControlService>.Instance);

            return new TestEnvironment(
                database,
                agent,
                certificate,
                sessions,
                operationLock,
                authority,
                agents,
                certificates,
                audit,
                provisioning,
                control);
        }

        public Task StartSession() => Control.Session(
            new BlockingAgentStream(Agent.Id),
            new TestResponseStream(),
            new TestServerCallContext());

        public ValueTask DisposeAsync() => _database.DisposeAsync();
    }

    /// <summary>
    /// Holds the first acquired lease until the test releases it. This makes the
    /// interleaving deterministic: the first operation is inside the critical
    /// section, and the second call provably waits for the lock.
    /// </summary>
    private sealed class HoldFirstOperationLock(IAgentOperationLock inner) : IAgentOperationLock
    {
        private readonly TaskCompletionSource _firstLeaseHeld =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource _releaseFirstLease =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly TaskCompletionSource _secondAcquireStarted =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private int _acquireCalls;

        public Task FirstLeaseHeld => _firstLeaseHeld.Task;

        public Task SecondAcquireStarted => _secondAcquireStarted.Task;

        public void ReleaseFirstLease() => _releaseFirstLease.TrySetResult();

        public async Task<IAsyncDisposable> AcquireAsync(
            Guid agentId,
            CancellationToken cancellationToken)
        {
            var call = Interlocked.Increment(ref _acquireCalls);
            if (call == 1)
            {
                var lease = await inner.AcquireAsync(agentId, cancellationToken);
                _firstLeaseHeld.TrySetResult();
                try
                {
                    await _releaseFirstLease.Task.WaitAsync(Timeout);
                }
                catch
                {
                    await lease.DisposeAsync();
                    throw;
                }

                return lease;
            }

            if (call == 2)
            {
                _secondAcquireStarted.TrySetResult();
            }

            return await inner.AcquireAsync(agentId, cancellationToken);
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
                        Name = "serialized-agent",
                        Version = "test",
                    },
                };
                return true;
            }

            await Task.Delay(System.Threading.Timeout.InfiniteTimeSpan, cancellationToken);
            return false;
        }
    }

    private sealed class TestResponseStream : IServerStreamWriter<ControlMessage>
    {
        public WriteOptions? WriteOptions { get; set; }

        public Task WriteAsync(ControlMessage message) => Task.CompletedTask;
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
                System.Security.Cryptography.SHA256.HashData(
                    System.Text.Encoding.UTF8.GetBytes(serial)));
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
        private int _consumed;

        public EnrollmentToken? Record { get; init; }

        public Task AddAsync(EnrollmentToken token, CancellationToken cancellationToken) =>
            Task.CompletedTask;

        public Task<EnrollmentToken?> FindByHashAsync(
            string tokenHash,
            CancellationToken cancellationToken) =>
            Task.FromResult(Record);

        public Task<bool> ConsumeAsync(Guid id, CancellationToken cancellationToken) =>
            Task.FromResult(
                Record is not null &&
                Record.Id == id &&
                Interlocked.Exchange(ref _consumed, 1) == 0);
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
}
