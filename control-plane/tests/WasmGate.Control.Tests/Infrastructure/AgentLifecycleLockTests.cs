using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests the production registry and per-agent lifecycle lock.</summary>
public sealed class AgentLifecycleLockTests
{
    [Fact]
    public void UnregisterDoesNotRemoveANewerSession()
    {
        var registry = new InMemoryAgentSessionRegistry();
        var agentId = Guid.NewGuid();
        var first = new TestSession(agentId);
        var second = new TestSession(agentId);

        Assert.Null(registry.Register(agentId, first));
        Assert.Same(first, registry.Register(agentId, second));
        registry.Unregister(agentId, first);

        Assert.True(registry.TryGet(agentId, out var current));
        Assert.Same(second, current);
    }

    [Fact]
    public async Task SameAgentOperationsAreSerialized()
    {
        var lifecycle = new AgentOperationLock();
        var agentId = Guid.NewGuid();
        var entered = 0;
        var maximum = 0;
        var firstMayLeave = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var secondEntered = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);

        async Task RunAsync(bool first)
        {
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            var now = Interlocked.Increment(ref entered);
            InterlockedMax(ref maximum, now);
            if (first)
            {
                await firstMayLeave.Task;
            }
            else
            {
                secondEntered.TrySetResult();
            }

            Interlocked.Decrement(ref entered);
        }

        var firstTask = RunAsync(first: true);
        await Task.Delay(20);
        var secondTask = RunAsync(first: false);
        await Task.Delay(50);
        Assert.False(secondEntered.Task.IsCompleted);

        firstMayLeave.TrySetResult();
        await Task.WhenAll(firstTask, secondTask);
        Assert.Equal(1, maximum);
        Assert.True(secondEntered.Task.IsCompleted);
    }

    [Fact]
    public async Task DifferentAgentsCanProceedIndependently()
    {
        var lifecycle = new AgentOperationLock();
        var entered = new ConcurrentBag<Guid>();
        var bothEntered = new TaskCompletionSource(
            TaskCreationOptions.RunContinuationsAsynchronously);
        var enteredCount = 0;

        async Task RunAsync(Guid agentId)
        {
            await using var lease = await lifecycle.AcquireAsync(
                agentId,
                CancellationToken.None);
            entered.Add(agentId);
            if (Interlocked.Increment(ref enteredCount) == 2)
            {
                bothEntered.TrySetResult();
            }

            await bothEntered.Task.WaitAsync(TimeSpan.FromSeconds(2));
        }

        await Task.WhenAll(RunAsync(Guid.NewGuid()), RunAsync(Guid.NewGuid()));
        Assert.Equal(2, entered.Count);
    }

    private static void InterlockedMax(ref int location, int value)
    {
        while (true)
        {
            var current = Volatile.Read(ref location);
            if (value <= current || Interlocked.CompareExchange(ref location, value, current) == current)
            {
                return;
            }
        }
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
