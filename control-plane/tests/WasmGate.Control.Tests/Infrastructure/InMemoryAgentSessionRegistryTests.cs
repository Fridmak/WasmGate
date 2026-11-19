using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Direct tests for the production session-registry lifecycle.</summary>
public sealed class InMemoryAgentSessionRegistryTests
{
    [Fact]
    public void TryRestoreRestoresOnlyAnUnretiredDisplacedSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);

        Assert.Null(registry.Register(agentId, first));
        Assert.Same(first, registry.Register(agentId, second));
        Assert.True(registry.TryRestore(agentId, second, first));
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(first, current);
    }

    [Fact]
    public void TryRestoreRejectsWhenAnotherSessionTookOver()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);
        var third = new TestSession(agentId);

        registry.Register(agentId, first);
        registry.Register(agentId, second);
        registry.Register(agentId, third);

        Assert.False(registry.TryRestore(agentId, second, first));
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(third, current);
    }

    [Fact]
    public void TryRestoreRejectsRetiredDisplacedSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);

        registry.Register(agentId, first);
        registry.Register(agentId, second);
        registry.Retire(first);

        Assert.False(registry.TryRestore(agentId, second, first));
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(second, current);
    }

    [Fact]
    public void RetiringAClosedDisplacedSessionCannotLeaveItCurrent()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);
        var third = new TestSession(agentId);

        registry.Register(agentId, first);
        registry.Register(agentId, second);
        registry.Retire(first);
        first.CloseAsync(CancellationToken.None);

        Assert.False(registry.TryRestore(agentId, second, first));
        Assert.Same(second, registry.Register(agentId, third));
        registry.Retire(second);
        second.CloseAsync(CancellationToken.None);
        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(third, current);
    }

    private sealed class TestSession(Guid agentId) : IAgentSession
    {
        public Guid AgentId { get; } = agentId;

        public ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;

        public ValueTask CloseAsync(CancellationToken cancellationToken) =>
            ValueTask.CompletedTask;
    }
}
